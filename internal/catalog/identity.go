package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"

	"github.com/srgsf/adsvc/internal/record"
)

// Share says which of this node's writes become records that peers receive.
type Share struct {
	Ads, Labels, Votes, FileMaps bool
}

// ShareAll shares everything (the default).
var ShareAll = Share{Ads: true, Labels: true, Votes: true, FileMaps: true}

// Policy is this node's view of other origins (trust.* in the config). Per-origin weights
// and blocks are in the origin_policy table (SetOrigins).
type Policy struct {
	SelfWeight    float64 // weight of this node's votes, ads and detections
	UnknownWeight float64 // weight of an origin without its own
	MinTrust      float64 // an ad is tracked from this trust up
	FileMapMin    float64 // summed weight a peer detection needs to be reported
	MaxAds        int     // the tracking set holds at most this many ads
	QuotaPerDay   int     // records accepted per origin per 24 h
}

// DefaultPolicy is the policy without a config file.
var DefaultPolicy = Policy{SelfWeight: 10, UnknownWeight: 0.2, MinTrust: 1, FileMapMin: 1, MaxAds: 1 << 18, QuotaPerDay: 5000}

// identity is this node's signing state.
type identity struct {
	id    *record.Identity
	clock *record.Clock
}

// SetIdentity makes id this node's identity: its records are signed with it. It restores
// the sequence and clock of earlier runs and makes unpublished local ads this node's. It
// publishes nothing: what was held back while sharing was off is published by SetShare,
// once the node's sharing policy is known.
func (db *DB) SetIdentity(ctx context.Context, id *record.Identity) error {
	var last int64
	if v, ok, err := db.Meta(ctx, "clock"); err != nil {
		return err
	} else if ok {
		last, _ = strconv.ParseInt(v, 10, 64)
	}
	var maxTS sql.NullInt64
	if err := db.r.QueryRowContext(ctx, `SELECT MAX(ts) FROM records WHERE origin = ?`, id.Origin[:]).Scan(&maxTS); err != nil {
		return err
	}
	db.self.Store(&identity{id: id, clock: record.NewClock(max(last, maxTS.Int64))})
	if err := db.SetMeta(ctx, "node_id", id.Origin.String()); err != nil {
		return err
	}
	if _, err := db.w.ExecContext(ctx, `UPDATE ads SET author = ?, author_ts = created WHERE author IS NULL`, id.Origin[:]); err != nil {
		return fmt.Errorf("claiming local ads: %w", err)
	}
	return setLabels(ctx, db.w, id.Origin, "1") // this node's own labels win now
}

// Self is this node's origin (zero before SetIdentity).
func (db *DB) Self() record.Origin {
	if s := db.self.Load(); s != nil {
		return s.id.Origin
	}
	return record.Origin{}
}

// Clock is this node's clock (nil before SetIdentity).
func (db *DB) Clock() *record.Clock {
	if s := db.self.Load(); s != nil {
		return s.clock
	}
	return nil
}

// Identity is this node's identity (nil before SetIdentity).
func (db *DB) Identity() *record.Identity {
	if s := db.self.Load(); s != nil {
		return s.id
	}
	return nil
}

// SetShare sets what this node publishes; newly shared ads are published right away.
func (db *DB) SetShare(ctx context.Context, s Share) error {
	db.share.Store(&s)
	return db.publishPending(ctx)
}

func (db *DB) shares() Share {
	if s := db.share.Load(); s != nil {
		return *s
	}
	return ShareAll
}

// SetPolicy sets the trust policy.
func (db *DB) SetPolicy(p Policy) { db.policy.Store(&p) }

// Policy is the trust policy in use.
func (db *DB) Policy() Policy {
	if p := db.policy.Load(); p != nil {
		return *p
	}
	return DefaultPolicy
}

// OriginRule is the config file's opinion of one origin.
type OriginRule struct {
	Origin  record.Origin
	Name    string
	Weight  *float64 // nil = the default for unknown origins
	Blocked bool
}

// SetOrigins replaces the origins managed by the config file with rules; origins set
// otherwise (the admin page) are kept.
func (db *DB) SetOrigins(ctx context.Context, rules []OriginRule) (err error) {
	defer wrap(&err, "setting origins")
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	if _, err := tx.ExecContext(ctx, `DELETE FROM origin_policy WHERE managed = 1`); err != nil {
		return err
	}
	for _, r := range rules {
		var w sql.NullFloat64
		if r.Weight != nil {
			w = sql.NullFloat64{Float64: *r.Weight, Valid: true}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO origin_policy (origin, name, weight, blocked, managed)
			VALUES (?, ?, ?, ?, 1) ON CONFLICT (origin) DO UPDATE SET name = excluded.name, weight = excluded.weight,
			blocked = excluded.blocked, managed = 1`, r.Origin[:], r.Name, w, r.Blocked); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// weights is the weight of every origin with its own policy, and the blocked ones.
func weights(ctx context.Context, q querier) (map[record.Origin]float64, map[record.Origin]bool, error) {
	w, blocked := map[record.Origin]float64{}, map[record.Origin]bool{}
	err := eachRowQ(ctx, q, `SELECT origin, weight, blocked FROM origin_policy`, nil, func(rows *sql.Rows) error {
		var o []byte
		var weight sql.NullFloat64
		var b bool
		if err := rows.Scan(&o, &weight, &b); err != nil {
			return err
		}
		var origin record.Origin
		copy(origin[:], o)
		if b {
			blocked[origin] = true
		}
		if weight.Valid {
			w[origin] = weight.Float64
		}
		return nil
	})
	return w, blocked, err
}

// weigher is the weight of one origin under a policy.
type weigher struct {
	self    record.Origin
	pol     Policy
	w       map[record.Origin]float64
	blocked map[record.Origin]bool
}

func (db *DB) weigher(ctx context.Context, q querier) (*weigher, error) {
	w, b, err := weights(ctx, q)
	if err != nil {
		return nil, err
	}
	return &weigher{self: db.Self(), pol: db.Policy(), w: w, blocked: b}, nil
}

func (w *weigher) of(o record.Origin) float64 {
	switch {
	case o == w.self || o == record.Origin{}:
		return w.pol.SelfWeight
	case w.blocked[o]:
		return 0
	}
	if v, ok := w.w[o]; ok {
		return v
	}
	return w.pol.UnknownWeight
}
