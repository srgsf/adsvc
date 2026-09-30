package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/srgsf/adsvc/internal/record"
)

// derived and local tables: a snapshot drops them. Derived ones are rebuilt from the
// records by whoever imports it; local ones are this node's own (its opinion of origins,
// its peers, its file maps, which are its watch history, and its users' ad reports, which
// hold URLs).
var snapshotDrop = []string{
	"peer_detections", "peer_aliases", "peer_maps", "ad_labels", "ad_types", "votes", "dups", "retractions", "ad_adds",
	"detections", "file_aliases", "file_maps", "ads", "ad_records", "origin_policy", "peers", "pushers", "pins", "published_maps",
	"reports",
}

// vacuumInto copies the catalogue to path. On disk it reads through a connection of its
// own (the pool's are query_only, which VACUUM INTO is not): in WAL mode it sees one
// consistent state and does not hold up the writer.
func (db *DB) vacuumInto(ctx context.Context, path string) error {
	c := db.w
	if db.uri != "" {
		var err error
		if c, err = sql.Open("sqlite", dsn(db.uri, []string{"busy_timeout(5000)"}, url.Values{"mode": {"ro"}})); err != nil {
			return err
		}
		defer c.Close()
	}
	_, err := c.ExecContext(ctx, `VACUUM INTO ?`, path)
	return err
}

// Snapshot writes the records of the catalogue to a new database file at path: every
// record this node accepted, and the node's id, nothing else. It is what /sync/snapshot
// hands out and ImportSnapshot reads.
func (db *DB) Snapshot(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("snapshot %s: file exists", path)
	}
	if err := db.vacuumInto(ctx, path); err != nil {
		return fmt.Errorf("snapshot %s: %w", path, err)
	}
	s, err := sql.Open("sqlite", dsn("file:"+path, []string{"foreign_keys(0)"}, nil))
	if err != nil {
		os.Remove(path)
		return err
	}
	defer s.Close()
	s.SetMaxOpenConns(1)
	err = func() error {
		for _, t := range snapshotDrop {
			if _, err := s.ExecContext(ctx, `DELETE FROM `+t); err != nil { //nolint:gosec // G202: table names are constants
				return err
			}
		}
		if _, err := s.ExecContext(ctx, `DELETE FROM meta WHERE key != 'node_id'`); err != nil {
			return err
		}
		if _, err := s.ExecContext(ctx, `UPDATE records SET via = NULL, received = 0`); err != nil {
			return err // how this node came by a record is its own business
		}
		_, err := s.ExecContext(ctx, `VACUUM`)
		return err
	}()
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("snapshot %s: %w", path, err)
	}
	return nil
}

// ImportSnapshot ingests the records of the snapshot at path, as coming from the node
// that made it. Every record is checked as if a peer had sent it.
func (db *DB) ImportSnapshot(ctx context.Context, path string) (IngestResult, error) {
	res := IngestResult{Rejected: map[string]int{}}
	if _, err := os.Stat(path); err != nil {
		return res, err
	}
	snap, err := Open(ctx, path)
	if err != nil {
		return res, err
	}
	defer snap.Close()
	v, ok, err := snap.Meta(ctx, "node_id")
	if err != nil {
		return res, err
	}
	if !ok {
		return res, errors.New("snapshot: no node id: not an adsvc snapshot")
	}
	via, err := record.ParseOrigin(v)
	if err != nil {
		return res, fmt.Errorf("snapshot: %w", err)
	}
	for after := int64(0); ; {
		es, err := snap.Log(ctx, after, 1000)
		if err != nil {
			return res, err
		}
		if len(es) == 0 {
			return res, nil
		}
		recs := make([]record.Record, len(es))
		for i, e := range es {
			recs[i] = e.Record
		}
		r, err := db.Ingest(ctx, recs, via)
		if err != nil {
			return res, err
		}
		res.Accepted += r.Accepted
		res.Duplicate += r.Duplicate
		for k, n := range r.Rejected {
			res.Rejected[k] += n
		}
		after = es[len(es)-1].ID
	}
}

func nowMs() int64 { return time.Now().UnixMilli() }

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

func jsonString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
