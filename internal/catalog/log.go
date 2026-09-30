package catalog

import (
	"context"
	"database/sql"
	"errors"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// Entry is a record in this node's log.
type Entry struct {
	ID  int64         // position in the log (the cursor)
	Via record.Origin // the node it came from; zero if written here
	record.Record
}

// Log returns up to limit records after position after, in log order.
func (db *DB) Log(ctx context.Context, after int64, limit int) ([]Entry, error) {
	var out []Entry
	err := db.EachLog(ctx, after, limit, func(e *Entry) error {
		e.Body = append([]byte(nil), e.Body...)
		out = append(out, *e)
		return nil
	})
	return out, err
}

// EachLog calls fn with up to limit records after position after, in log order, as they
// are read: a page of the log is never held in memory whole. The entry (its Body too) is
// only valid during the call.
func (db *DB) EachLog(ctx context.Context, after int64, limit int, fn func(*Entry) error) error {
	var e Entry
	var o, sig, via, body sql.RawBytes
	return db.eachRow(ctx, `SELECT id, kind, origin, seq, ts, body, sig, via FROM records WHERE id > ? ORDER BY id LIMIT ?`,
		[]any{after, limit}, func(rows *sql.Rows) error {
			if err := rows.Scan(&e.ID, &e.Kind, &o, &e.Seq, &e.TS, &body, &sig, &via); err != nil {
				return err
			}
			e.Via = record.Origin{}
			copy(e.Origin[:], o)
			copy(e.Via[:], via)
			copy(e.Sig[:], sig)
			e.Body = body
			return fn(&e)
		})
}

// Head is the position of the last record in the log (0 if none).
func (db *DB) Head(ctx context.Context) (int64, error) {
	var n sql.NullInt64
	err := db.r.QueryRowContext(ctx, `SELECT MAX(id) FROM records`).Scan(&n)
	return n.Int64, err
}

// AdRecord returns the ad.add record the ad's metadata comes from, or nil.
func (db *DB) AdRecord(ctx context.Context, id fingerprint.ID) (*record.Record, error) {
	var r record.Record
	var o, sig []byte
	err := db.r.QueryRowContext(ctx, `SELECT r.kind, r.origin, r.seq, r.ts, r.body, r.sig FROM ads a
		JOIN records r ON r.id = a.record WHERE a.id = ?`, id[:]).Scan(&r.Kind, &o, &r.Seq, &r.TS, &r.Body, &sig)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	copy(r.Origin[:], o)
	copy(r.Sig[:], sig)
	return &r, nil
}
