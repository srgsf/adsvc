package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// PeerState is what this node remembers of a peer.
type PeerState struct {
	Name                   string
	NodeID                 record.Origin
	PullCursor, PushCursor int64
}

// Peer returns the state of the peer called name (zero cursors if it is new).
func (db *DB) Peer(ctx context.Context, name string) (_ PeerState, err error) {
	defer wrap(&err, "reading peer %s", name)
	p := PeerState{Name: name}
	var node []byte
	err = db.r.QueryRowContext(ctx, `SELECT node_id, pull_cursor, push_cursor FROM peers WHERE name = ?`, name).
		Scan(&node, &p.PullCursor, &p.PushCursor)
	if errors.Is(err, sql.ErrNoRows) {
		return p, nil
	}
	copy(p.NodeID[:], node)
	return p, err
}

// SavePeer stores a peer's state after a sync: its cursors, the outcome, what it sent.
func (db *DB) SavePeer(ctx context.Context, p PeerState, syncErr error, res IngestResult) (err error) {
	defer wrap(&err, "saving peer %s", p.Name)
	errText := ""
	if syncErr != nil {
		errText = syncErr.Error()
	}
	var node any
	if p.NodeID != (record.Origin{}) {
		node = p.NodeID[:]
	}
	return db.write(ctx, func(tx *sql.Tx) error {
		var rejected string
		err := tx.QueryRowContext(ctx, `SELECT rejected FROM peers WHERE name = ?`, p.Name).Scan(&rejected)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		counts := map[string]int{}
		if rejected != "" {
			_ = jsonUnmarshal(rejected, &counts)
		}
		for k, n := range res.Rejected {
			counts[k] += n
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO peers (name, node_id, pull_cursor, push_cursor, last_ok, last_error, received, rejected)
			VALUES (:name, :node, :pull, :push, CASE WHEN :err = '' THEN :now END, :err, :recv, :rej)
			ON CONFLICT (name) DO UPDATE SET node_id = COALESCE(excluded.node_id, peers.node_id),
			pull_cursor = excluded.pull_cursor, push_cursor = excluded.push_cursor,
			last_ok = COALESCE(excluded.last_ok, peers.last_ok), last_error = excluded.last_error,
			received = peers.received + excluded.received, rejected = excluded.rejected`,
			sql.Named("name", p.Name), sql.Named("node", node), sql.Named("pull", p.PullCursor), sql.Named("push", p.PushCursor),
			sql.Named("err", errText), sql.Named("now", nowMs()), sql.Named("recv", res.Accepted), sql.Named("rej", jsonString(counts)))
		return err
	})
}

// PeerRow is a configured peer as this node last synced with it.
type PeerRow struct {
	Name                   string
	NodeID                 record.Origin // zero until the first sync
	PullCursor, PushCursor int64
	LastOK                 time.Time // zero if never
	LastError              string
	Received               int            // records accepted from its log
	Rejected               map[string]int // records of its log rejected, by reason
	Relayed, RelayedTrash  int            // ads that came through it (any way), and how many are trash
}

// PusherRow is a node that pushed records to this one.
type PusherRow struct {
	NodeID                record.Origin
	LastPush              time.Time
	Received, Duplicate   int
	Rejected              map[string]int // by reason
	Relayed, RelayedTrash int            // as in PeerRow
}

// Peers lists the peers this node synced with and the nodes that pushed to it. Trash is
// as in Origins.
func (db *DB) Peers(ctx context.Context) (peers []PeerRow, pushers []PusherRow, err error) {
	err = db.View(ctx, func(v View) error {
		q := v.tx
		w, err := db.weigher(ctx, q)
		if err != nil {
			return err
		}
		trash, _, err := db.trash(ctx, q, w)
		if err != nil {
			return err
		}
		relayed, relayedTrash := map[record.Origin]int{}, map[record.Origin]int{}
		if err := eachRowQ(ctx, q, `SELECT DISTINCT r.via, ar.ad FROM records r JOIN ad_records ar ON ar.record = r.id
			WHERE r.kind = 'ad.add' AND r.via IS NOT NULL`, nil, func(rs *sql.Rows) error {
			var via, ad []byte
			if err := rs.Scan(&via, &ad); err != nil {
				return err
			}
			o := record.Origin(via)
			relayed[o]++
			relayedTrash[o] += b2i(trash[fingerprint.ID(ad)])
			return nil
		}); err != nil {
			return err
		}
		if err := eachRowQ(ctx, q, `SELECT name, node_id, pull_cursor, push_cursor, last_ok, last_error, received, rejected
			FROM peers ORDER BY name`, nil, func(rs *sql.Rows) error {
			var p PeerRow
			var node []byte
			var lastOK sql.NullInt64
			var rejected string
			if err := rs.Scan(&p.Name, &node, &p.PullCursor, &p.PushCursor, &lastOK, &p.LastError, &p.Received, &rejected); err != nil {
				return err
			}
			copy(p.NodeID[:], node)
			if lastOK.Valid {
				p.LastOK = time.UnixMilli(lastOK.Int64)
			}
			p.Rejected = map[string]int{}
			_ = jsonUnmarshal(rejected, &p.Rejected)
			if p.NodeID != (record.Origin{}) {
				p.Relayed, p.RelayedTrash = relayed[p.NodeID], relayedTrash[p.NodeID]
			}
			peers = append(peers, p)
			return nil
		}); err != nil {
			return err
		}
		return eachRowQ(ctx, q, `SELECT node_id, last_push, received, duplicate, rejected FROM pushers ORDER BY last_push DESC`,
			nil, func(rs *sql.Rows) error {
				var p PusherRow
				var node []byte
				var last int64
				var rejected string
				if err := rs.Scan(&node, &last, &p.Received, &p.Duplicate, &rejected); err != nil {
					return err
				}
				copy(p.NodeID[:], node)
				p.LastPush = time.UnixMilli(last)
				p.Rejected = map[string]int{}
				_ = jsonUnmarshal(rejected, &p.Rejected)
				p.Relayed, p.RelayedTrash = relayed[p.NodeID], relayedTrash[p.NodeID]
				pushers = append(pushers, p)
				return nil
			})
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing peers: %w", err)
	}
	return peers, pushers, nil
}

// CountPush counts what became of the records node pushed to this node.
func (db *DB) CountPush(ctx context.Context, node record.Origin, res IngestResult) (err error) {
	defer wrap(&err, "counting a push")
	return db.write(ctx, func(tx *sql.Tx) error {
		var rejected string
		err := tx.QueryRowContext(ctx, `SELECT rejected FROM pushers WHERE node_id = ?`, node[:]).Scan(&rejected)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		counts := map[string]int{}
		if rejected != "" {
			_ = jsonUnmarshal(rejected, &counts)
		}
		for k, n := range res.Rejected {
			counts[k] += n
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO pushers (node_id, last_push, received, duplicate, rejected) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (node_id) DO UPDATE SET last_push = excluded.last_push, received = pushers.received + excluded.received,
			duplicate = pushers.duplicate + excluded.duplicate, rejected = excluded.rejected`,
			node[:], nowMs(), res.Accepted, res.Duplicate, jsonString(counts))
		return err
	})
}
