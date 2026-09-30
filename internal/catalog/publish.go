package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// publish signs a record of kind with body, stores it and materializes it, in tx.
func (db *DB) publish(ctx context.Context, tx *sql.Tx, kind string, body any, ts int64) (int64, error) {
	s := db.self.Load()
	if s == nil {
		return 0, errors.New("catalogue: no identity to sign with")
	}
	var seq uint64
	var v sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'self_seq'`).Scan(&v); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if v.Valid {
		seq, _ = strconv.ParseUint(v.String, 10, 64)
	}
	seq++
	if ts == 0 {
		ts = s.clock.Next()
	}
	r, err := record.New(kind, seq, ts, body)
	if err != nil {
		return 0, err
	}
	s.id.Sign(&r)
	decoded, err := record.CheckDecode(&r, time.Now()) // what peers would reject is not published
	if err != nil {
		return 0, err
	}
	id, err := insertRecord(ctx, tx, &r, nil)
	if err != nil {
		return 0, err
	}
	if err := db.materialize(ctx, tx, &r, decoded, id); err != nil {
		return 0, err
	}
	for k, val := range map[string]string{"self_seq": strconv.FormatUint(seq, 10), "clock": strconv.FormatInt(s.clock.Last(), 10)} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, k, val); err != nil {
			return 0, err
		}
	}
	return id, nil
}

func insertRecord(ctx context.Context, tx *sql.Tx, r *record.Record, via *record.Origin) (int64, error) {
	h := r.Hash()
	var v any
	if via != nil {
		v = via[:]
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO records (hash, origin, seq, kind, ts, body, sig, via, received)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, h[:], r.Origin[:], r.Seq, r.Kind, r.TS, r.Body, r.Sig[:], v, time.Now().UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("storing record %s/%d: %w", r.Origin.Short(), r.Seq, err)
	}
	return res.LastInsertId()
}

// wrap prefixes *err, if any, with the operation that failed: the one wrap of an exported
// write, whose internals return bare errors.
func wrap(err *error, format string, args ...any) {
	if *err != nil {
		*err = fmt.Errorf(format+": %w", append(args, *err)...)
	}
}

// write runs fn in a write transaction.
func (db *DB) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	defer writeSeconds.Since(time.Now())
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// AddAd stores an ad enrolled on this node, with its encoded landmarks, and publishes it
// when ads are shared. Adding an ad that exists (same id) changes nothing and reports
// added = false, unless this node had withdrawn it (or voted it down): then it is back,
// as a new ad.add (or a vote for it), because the same audio is the same ad.
func (db *DB) AddAd(ctx context.Context, a Ad, points []byte) (added bool, err error) {
	if db.self.Load() == nil {
		return db.PutAd(ctx, a, points)
	}
	if !db.shares().Ads {
		if added, err = db.PutAd(ctx, a, points); err != nil || added {
			return added, err
		}
		return false, db.write(ctx, func(tx *sql.Tx) error { return db.restore(ctx, tx, a, points) })
	}
	err = db.write(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM ads WHERE id = ?`, a.ID[:]).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return db.restore(ctx, tx, a, points)
		}
		_, err := db.publish(ctx, tx, record.KindAdAdd, adAddBody(a, points), 0)
		added = err == nil
		return err
	})
	return added, err
}

// restore undoes this node's withdrawal of a stored ad: its own retraction is overtaken
// by a new ad.add (only when ads are shared; otherwise the retraction is dropped, the ad
// was never shared), and a vote against another origin's ad becomes one for it.
func (db *DB) restore(ctx context.Context, tx *sql.Tx, a Ad, points []byte) error {
	self := db.Self()
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM retractions WHERE ad = :ad AND origin = :self AND `+retractionLive,
		sql.Named("ad", a.ID[:]), sql.Named("self", self[:])).Scan(&live); err != nil {
		return err
	}
	if live > 0 {
		if !db.shares().Ads {
			_, err := tx.ExecContext(ctx, `DELETE FROM retractions WHERE ad = ? AND origin = ?`, a.ID[:], self[:])
			return err
		}
		_, err := db.publish(ctx, tx, record.KindAdAdd, adAddBody(a, points), 0)
		return err
	}
	var v int
	err := tx.QueryRowContext(ctx, `SELECT value FROM votes WHERE ad = ? AND origin = ?`, a.ID[:], self[:]).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) || err == nil && v >= 0 {
		return nil
	}
	if err != nil {
		return err
	}
	return db.vote(ctx, tx, record.AdVote{Ad: a.ID, Value: 1, Reason: record.ReasonGood})
}

func adAddBody(a Ad, points []byte) record.AdAdd {
	b := record.AdAdd{ID: a.ID, FPVersion: a.FPVersion, DurationMs: a.DurationMs, Points: points, Label: a.Label, Type: adtype.Of(a.Type)}
	if a.Source != nil {
		b.Source = &record.Source{Key: a.Source.Key, StartMs: a.Source.StartMs, EndMs: a.Source.EndMs}
	}
	return b
}

// publishPending publishes this node's ads that are to be shared and have no record yet
// (added while not shared, or before identities existed).
func (db *DB) publishPending(ctx context.Context) error {
	if db.self.Load() == nil || !db.shares().Ads {
		return nil
	}
	self := db.Self()
	var todo []fingerprint.ID
	if err := db.eachRow(ctx, `SELECT id FROM ads WHERE author = ? AND record IS NULL AND fp_version = ?`,
		[]any{self[:], fingerprint.Version}, func(rows *sql.Rows) error {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return err
			}
			var id fingerprint.ID
			copy(id[:], b)
			todo = append(todo, id)
			return nil
		}); err != nil {
		return err
	}
	for _, id := range todo {
		if err := db.write(ctx, func(tx *sql.Tx) error {
			a, pts, err := adByID(ctx, tx, id)
			if err != nil || a == nil {
				return err
			}
			_, err = db.publish(ctx, tx, record.KindAdAdd, adAddBody(*a, pts), 0)
			return err
		}); err != nil {
			if _, ok := errors.AsType[*record.Rejection](err); !ok {
				return fmt.Errorf("publishing ad %s: %w", id, err)
			}
			slog.Warn("local ad not published", "ad", id, "err", err) // stays local
		}
	}
	return nil
}

func adByID(ctx context.Context, q querier, id fingerprint.ID) (*Ad, []byte, error) {
	var a Ad
	var pts []byte
	var created int64
	var key sql.NullString
	var start, end sql.NullInt32
	err := q.QueryRowContext(ctx, `SELECT fp_version, label, type, duration_ms, n_points, points, created, source_key,
		source_start_ms, source_end_ms FROM ads WHERE id = ?`, id[:]).Scan(&a.FPVersion, &a.Label, &a.Type, &a.DurationMs,
		&a.NPoints, &pts, &created, &key, &start, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	a.ID, a.Created = id, time.UnixMilli(created)
	if key.Valid {
		a.Source = &Source{Key: key.String, StartMs: start.Int32, EndMs: end.Int32}
	}
	return &a, pts, nil
}

// Retract takes an ad out of this node's library: its author withdraws it (ad.retract),
// anyone else votes it down as not an ad. Its detections leave this node's file maps.
func (db *DB) Retract(ctx context.Context, id fingerprint.ID) (_ bool, err error) {
	defer wrap(&err, "retracting ad %s", id)
	var found bool
	err = db.write(ctx, func(tx *sql.Tx) error {
		var author []byte
		err := tx.QueryRowContext(ctx, `SELECT author FROM ads WHERE id = ?`, id[:]).Scan(&author)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		self := db.Self()
		if _, err := tx.ExecContext(ctx, `DELETE FROM detections WHERE ad = ?`, id[:]); err != nil {
			return err
		}
		if db.self.Load() == nil {
			_, err := tx.ExecContext(ctx, `DELETE FROM ads WHERE id = ?`, id[:])
			return err
		}
		if author == nil || record.Origin(author) == self {
			_, err := db.publish(ctx, tx, record.KindAdRetract, record.AdRetract{Ad: id}, 0)
			return err
		}
		return db.vote(ctx, tx, record.AdVote{Ad: id, Value: -1, Reason: record.ReasonNotAd})
	})
	return found, err
}

// Label labels an ad and sets its type (an adtype; "" leaves it as it is). This node's
// label and type win on this node.
func (db *DB) Label(ctx context.Context, id fingerprint.ID, label, typ string) (err error) {
	defer wrap(&err, "labelling ad %s", id)
	if typ != "" && !adtype.Valid(typ) {
		return fmt.Errorf("unknown type %q", typ)
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		body := record.AdLabel{Ad: id, Label: label, Type: typ}
		if db.shares().Labels {
			_, err := db.publish(ctx, tx, record.KindAdLabel, body, 0)
			return err
		}
		return db.upsertLabel(ctx, tx, body, db.Self(), db.Clock().Next(), 0)
	})
}

// Vote votes on an ad as this node.
func (db *DB) Vote(ctx context.Context, v record.AdVote) (err error) {
	defer wrap(&err, "voting on ad %s", v.Ad)
	return db.write(ctx, func(tx *sql.Tx) error { return db.vote(ctx, tx, v) })
}

func (db *DB) vote(ctx context.Context, tx *sql.Tx, v record.AdVote) error {
	if db.shares().Votes {
		_, err := db.publish(ctx, tx, record.KindAdVote, v, 0)
		return err
	}
	return upsertVote(ctx, tx, v, db.Self(), db.Clock().Next(), 0)
}

// Pin keeps an ad in the tracking set whatever its trust (on = false unpins).
func (db *DB) Pin(ctx context.Context, id fingerprint.ID, on bool) (err error) {
	defer wrap(&err, "pinning ad %s", id)
	q := `INSERT INTO pins (ad) VALUES (?) ON CONFLICT DO NOTHING`
	if !on {
		q = `DELETE FROM pins WHERE ad = ?`
	}
	_, err = db.w.ExecContext(ctx, q, id[:])
	return err
}

// PublishFileMap publishes what this node found in a file, when file maps are shared and
// it changed since the last time: its confirmed detections, never analysed ranges (they
// change with every block, and records are never deleted). The time is the start of the
// day, so a record does not tell when the file was watched.
func (db *DB) PublishFileMap(ctx context.Context, m *FileMap) error {
	body, digest, ok := db.fileMapBody(m)
	if !ok {
		return nil
	}
	return db.write(ctx, func(tx *sql.Tx) error { return db.publishFileMap(ctx, tx, m.Key, body, digest) })
}

// fileMapBody is the file.map record of m, and its digest; ok is false when there is
// nothing to publish (no identity, file maps not shared, no confirmed ad).
func (db *DB) fileMapBody(m *FileMap) (body record.FileMap, digest [sha256.Size]byte, ok bool) {
	if db.self.Load() == nil || !db.shares().FileMaps {
		return body, digest, false
	}
	body = record.FileMap{Key: m.Key, Aliases: m.Aliases, FPVersion: m.FPVersion, Size: m.Size, DurationMs: m.DurationMs, Ads: []record.Detection{}}
	for _, d := range m.Detections {
		if d.Confirmed {
			body.Ads = append(body.Ads, d.record())
		}
	}
	if len(body.Ads) == 0 {
		return body, digest, false
	}
	b, err := json.Marshal(body)
	if err != nil { // plain data: cannot fail
		return body, digest, false
	}
	return body, sha256.Sum256(b), true
}

// publishFileMap publishes body in tx unless the map published last under key has the
// same digest.
func (db *DB) publishFileMap(ctx context.Context, tx *sql.Tx, key string, body record.FileMap, digest [sha256.Size]byte) error {
	var old []byte
	err := tx.QueryRowContext(ctx, `SELECT digest FROM published_maps WHERE key = ?`, key).Scan(&old)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if slices.Equal(old, digest[:]) {
		return nil
	}
	day := time.Now().UTC().Truncate(24 * time.Hour).UnixMilli()
	if _, err := db.publish(ctx, tx, record.KindFileMap, body, day); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO published_maps (key, digest) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET digest = excluded.digest`, key, digest[:])
	return err
}
