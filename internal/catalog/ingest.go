package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/record"
)

// IngestResult is what Ingest did with a batch.
type IngestResult struct {
	Accepted, Duplicate int
	Rejected            map[string]int // by reason (record.Reject*)
}

// Ingest stores records received from via (a peer, or a snapshot's node), after checking
// each: signature and body (record.Check), blocked origins, the per-origin daily quota,
// and sequence numbers reused with other content. Accepted records are materialized.
// Records of this node's own origin (from a snapshot, after a reinstall) are stored but
// not materialized; they move this node's sequence past theirs.
func (db *DB) Ingest(ctx context.Context, recs []record.Record, via record.Origin) (IngestResult, error) {
	res := IngestResult{Rejected: map[string]int{}}
	now := time.Now()
	pol := db.Policy()
	self := db.Self()
	// The checks that need no database (signatures, bodies, landmarks) run before the
	// write transaction, which every other writer waits for.
	pre := make([]checked, len(recs))
	for i := range recs {
		pre[i] = check(&recs[i], now)
	}
	err := db.write(ctx, func(tx *sql.Tx) error {
		w, err := db.weigher(ctx, tx)
		if err != nil {
			return err
		}
		a, err := newAcceptor(ctx, tx, w, pol, now)
		if err != nil {
			return err
		}
		defer a.close()
		for i := range recs {
			r := &recs[i]
			reason, err := a.accept(ctx, r, &pre[i])
			if err != nil {
				return err
			}
			switch reason {
			case "":
			case "duplicate":
				res.Duplicate++
				continue
			default:
				res.Rejected[reason]++
				continue
			}
			id, err := insertRecord(ctx, tx, r, &via)
			if err != nil {
				return err
			}
			res.Accepted++
			if c := db.Clock(); c != nil {
				c.Observe(r.TS)
			}
			// A record of this node's own that it does not hold (a restored export): applied
			// like any other, and the next one follows it.
			if r.Origin == self && self != (record.Origin{}) {
				if _, err := tx.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('self_seq', ?)
					ON CONFLICT (key) DO UPDATE SET value = MAX(CAST(value AS INTEGER), CAST(excluded.value AS INTEGER))`,
					strconv.FormatUint(r.Seq, 10)); err != nil {
					return err
				}
			}
			if err := db.materialize(ctx, tx, r, pre[i].body, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		countIngest(res)
	}
	return res, err
}

// checked is what record.CheckDecode found about a record: its hash, and its decoded body
// or the reason it is rejected.
type checked struct {
	hash   [32]byte
	body   record.Body
	reason string // "" = valid
}

func check(r *record.Record, now time.Time) checked {
	c := checked{hash: r.Hash()}
	body, err := record.CheckDecode(r, now)
	switch rej, ok := errors.AsType[*record.Rejection](err); {
	case err == nil:
		c.body = body
	case ok:
		c.reason = rej.Reason
	default:
		c.reason = record.RejectInvalid
	}
	return c
}

// acceptor holds what Ingest's checks against the database need for one batch.
type acceptor struct {
	w     *weigher
	pol   Policy
	since int64 // the quota counts records received from here on (unix ms)
	bySeq *sql.Stmt
	quota *sql.Stmt
	used  map[record.Origin]int // records received per origin: counted once, then kept up
}

func newAcceptor(ctx context.Context, tx *sql.Tx, w *weigher, pol Policy, now time.Time) (*acceptor, error) {
	a := &acceptor{w: w, pol: pol, since: now.Add(-24 * time.Hour).UnixMilli(), used: map[record.Origin]int{}}
	var err error
	if a.bySeq, err = tx.PrepareContext(ctx, `SELECT hash FROM records WHERE origin = ? AND seq = ?`); err != nil {
		return nil, err
	}
	if a.quota, err = tx.PrepareContext(ctx, `SELECT COUNT(*) FROM records WHERE origin = ? AND received > ?`); err != nil {
		a.bySeq.Close()
		return nil, err
	}
	return a, nil
}

func (a *acceptor) close() {
	a.bySeq.Close()
	a.quota.Close()
}

// accept checks one record against the database; it returns "" to accept, "duplicate",
// or a rejection reason. An accepted record counts against its origin's quota.
func (a *acceptor) accept(ctx context.Context, r *record.Record, c *checked) (string, error) {
	// The hash covers origin and seq: the record at (origin, seq) is this one (a
	// duplicate), another one (a reused seq), or none.
	var have []byte
	err := a.bySeq.QueryRowContext(ctx, r.Origin[:], r.Seq).Scan(&have)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	stored := err == nil
	switch {
	case stored && bytes.Equal(have, c.hash[:]):
		return "duplicate", nil
	case c.reason != "":
		return c.reason, nil
	case a.w.blocked[r.Origin]:
		return record.RejectBlocked, nil
	case stored:
		return record.RejectInvalid, nil // a sequence number reused with other content
	}
	if r.Origin != a.w.self && a.pol.QuotaPerDay > 0 {
		n, ok := a.used[r.Origin]
		if !ok {
			if err := a.quota.QueryRowContext(ctx, r.Origin[:], a.since).Scan(&n); err != nil {
				return "", err
			}
		}
		if n >= a.pol.QuotaPerDay {
			a.used[r.Origin] = n
			return record.RejectQuota, nil
		}
		a.used[r.Origin] = n + 1
	}
	return "", nil
}

// materialize applies an accepted record to the tables it feeds. Every kind is applied so
// that the result does not depend on the order records arrive in.
// body is r's decoded body (record.CheckDecode).
func (db *DB) materialize(ctx context.Context, tx *sql.Tx, r *record.Record, body record.Body, id int64) error {
	if body == nil {
		return nil // unknown kinds are kept, not applied
	}
	var err error
	for _, ad := range body.AdIDs() {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ad_records (ad, record) VALUES (?, ?) ON CONFLICT DO NOTHING`, ad[:], id); err != nil {
			return fmt.Errorf("filing %s record %s/%d: %w", r.Kind, r.Origin.Short(), r.Seq, err)
		}
	}
	switch b := body.(type) {
	case *record.AdAdd:
		var key sql.NullString
		var start, end sql.NullInt32
		if b.Source != nil {
			key = sql.NullString{String: b.Source.Key, Valid: true}
			start = sql.NullInt32{Int32: b.Source.StartMs, Valid: true}
			end = sql.NullInt32{Int32: b.Source.EndMs, Valid: true}
		}
		// The earliest ad.add (by time, then origin) is the author's.
		_, err = tx.ExecContext(ctx, `INSERT INTO ads (id, fp_version, label, type, duration_ms, n_points, points, created,
			source_key, source_start_ms, source_end_ms, author, author_ts, record)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET label = excluded.label, type = excluded.type, source_key = excluded.source_key,
			source_start_ms = excluded.source_start_ms, source_end_ms = excluded.source_end_ms,
			author = excluded.author, author_ts = excluded.author_ts, record = excluded.record
			WHERE ads.author IS NULL OR (excluded.author_ts, excluded.author) < (ads.author_ts, ads.author)
				OR (ads.record IS NULL AND excluded.author = ads.author)`,
			b.ID[:], b.FPVersion, b.Label, adtype.Of(b.Type), b.DurationMs, b.Landmarks(), b.Points, r.TS, key, start, end, r.Origin[:], r.TS, id)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO ad_adds (ad, origin, ts) VALUES (?, ?, ?)
				ON CONFLICT (ad, origin) DO UPDATE SET ts = excluded.ts WHERE excluded.ts > ad_adds.ts`, b.ID[:], r.Origin[:], r.TS)
		}
		if err == nil {
			err = setLabels(ctx, tx, db.Self(), `a.id = :id`, sql.Named("id", b.ID[:]))
		}
	case *record.AdLabel:
		err = db.upsertLabel(ctx, tx, *b, r.Origin, r.TS, r.Seq)
	case *record.AdVote:
		err = upsertVote(ctx, tx, *b, r.Origin, r.TS, r.Seq)
	case *record.AdDup:
		_, err = tx.ExecContext(ctx, `INSERT INTO dups (ad, origin, canonical, ts, seq) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (ad, origin) DO UPDATE SET canonical = excluded.canonical, ts = excluded.ts, seq = excluded.seq
			WHERE (excluded.ts, excluded.seq) > (dups.ts, dups.seq)`, b.Ad[:], r.Origin[:], b.Canonical[:], r.TS, r.Seq)
	case *record.AdRetract:
		_, err = tx.ExecContext(ctx, `INSERT INTO retractions (ad, origin, ts) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			b.Ad[:], r.Origin[:], r.TS)
	case *record.FileMap:
		if r.Origin == db.Self() {
			err = restoreFileMap(ctx, tx, b, r) // this node's own maps are file_maps / detections
			break
		}
		err = upsertPeerMap(ctx, tx, b, r)
	}
	if err != nil {
		return fmt.Errorf("applying %s record %s/%d: %w", r.Kind, r.Origin.Short(), r.Seq, err)
	}
	return nil
}

// restoreFileMap stores a file map this node published but no longer holds (a restored
// export) as its own, unless it holds one at least as new under any of the map's keys: a
// map of a live session is always newer than the day its record is timed at. The record
// has the confirmed ads only, so the rest of the file is analysed again.
func restoreFileMap(ctx context.Context, tx *sql.Tx, b *record.FileMap, r *record.Record) error {
	for _, k := range append([]string{b.Key}, b.Aliases...) {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM file_maps WHERE updated > ? AND key IN (
			SELECT ? UNION ALL SELECT key FROM file_aliases WHERE alias = ?)`, r.TS, k, k).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
	m := &FileMap{Key: b.Key, Aliases: b.Aliases, FPVersion: b.FPVersion, Size: b.Size, DurationMs: b.DurationMs, Analyzed: b.Analyzed}
	for _, d := range b.Ads {
		m.Detections = append(m.Detections, detectionOf(d))
	}
	if err := putFileMap(ctx, tx, m, r.TS); err != nil {
		return err
	}
	digest := sha256.Sum256(r.Body) // what PublishFileMap compares, so it is not published again
	_, err := tx.ExecContext(ctx, `INSERT INTO published_maps (key, digest) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET digest = excluded.digest`, b.Key, digest[:])
	return err
}

func (db *DB) upsertLabel(ctx context.Context, tx *sql.Tx, b record.AdLabel, o record.Origin, ts int64, seq uint64) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO ad_labels (ad, origin, label, ts, seq) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (ad, origin) DO UPDATE SET label = excluded.label, ts = excluded.ts, seq = excluded.seq
		WHERE (excluded.ts, excluded.seq) > (ad_labels.ts, ad_labels.seq)`, b.Ad[:], o[:], b.Label, ts, seq); err != nil {
		return err
	}
	if b.Type != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO ad_types (ad, origin, type, ts, seq) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (ad, origin) DO UPDATE SET type = excluded.type, ts = excluded.ts, seq = excluded.seq
			WHERE (excluded.ts, excluded.seq) > (ad_types.ts, ad_types.seq)`, b.Ad[:], o[:], b.Type, ts, seq); err != nil {
			return err
		}
	}
	return setLabels(ctx, tx, db.Self(), `a.id = :id`, sql.Named("id", b.Ad[:]))
}

func upsertVote(ctx context.Context, tx *sql.Tx, b record.AdVote, o record.Origin, ts int64, seq uint64) error {
	var key sql.NullString
	var start sql.NullInt32
	if b.Key != "" {
		key = sql.NullString{String: b.Key, Valid: true}
	}
	if b.StartMs != nil {
		start = sql.NullInt32{Int32: *b.StartMs, Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO votes (ad, origin, value, reason, file_key, start_ms, ts, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT (ad, origin) DO UPDATE SET value = excluded.value,
		reason = excluded.reason, file_key = excluded.file_key, start_ms = excluded.start_ms, ts = excluded.ts,
		seq = excluded.seq WHERE (excluded.ts, excluded.seq) > (votes.ts, votes.seq)`,
		b.Ad[:], o[:], b.Value, b.Reason, key, start, ts, seq)
	return err
}

func upsertPeerMap(ctx context.Context, tx *sql.Tx, b *record.FileMap, r *record.Record) error {
	var ts, seq int64
	err := tx.QueryRowContext(ctx, `SELECT ts, seq FROM peer_maps WHERE key = ? AND origin = ?`, b.Key, r.Origin[:]).Scan(&ts, &seq)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case ts > r.TS || (ts == r.TS && uint64(seq) >= r.Seq):
		return nil // an older map of this origin
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM peer_maps WHERE key = ? AND origin = ?`, b.Key, r.Origin[:]); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO peer_maps (key, origin, fp_version, size, duration_ms, ts, seq)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, b.Key, r.Origin[:], b.FPVersion, b.Size, b.DurationMs, r.TS, r.Seq); err != nil {
		return err
	}
	for _, a := range b.Aliases {
		if a == b.Key {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO peer_aliases (alias, origin, key) VALUES (?, ?, ?)
			ON CONFLICT (alias, origin) DO UPDATE SET key = excluded.key`, a, r.Origin[:], b.Key); err != nil {
			return err
		}
	}
	for _, d := range b.Ads {
		if _, err := tx.ExecContext(ctx, `INSERT INTO peer_detections (key, origin, ad, start_ms, end_ms, score, confirmed)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, b.Key, r.Origin[:], d.Ad[:], d.StartMs, d.EndMs, d.Score, d.Confirmed); err != nil {
			return err
		}
	}
	return nil
}
