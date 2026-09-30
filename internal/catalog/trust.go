package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// Tracking is the tracking set under the policy: the ads to index, by id, each with its
// trust. See docs/database.md §4.
func (db *DB) Tracking(ctx context.Context) ([]Ad, error) { return db.tracking(ctx, db.r) }

// Tracking is DB.Tracking on the snapshot.
func (v View) Tracking(ctx context.Context) ([]Ad, error) { return v.db.tracking(ctx, v.tx) }

func (db *DB) tracking(ctx context.Context, q querier) ([]Ad, error) {
	all, err := ads(ctx, q, fingerprint.Version, db.Self())
	if err != nil {
		return nil, err
	}
	w, err := db.weigher(ctx, q)
	if err != nil {
		return nil, err
	}
	st, err := db.trust(ctx, q, w, all, false)
	if err != nil {
		return nil, err
	}
	return selectTracking(all, st, w.pol), nil
}

// selectTracking picks the tracking set out of all (ads of the current fingerprint
// version): active ads that are pinned or trusted enough, at most pol.MaxAds of them
// (pinned first, then by trust, then the newest), by id.
func selectTracking(all []Ad, st map[fingerprint.ID]*standing, pol Policy) []Ad {
	var out []Ad
	for _, a := range all {
		s := st[a.ID]
		a.Trust, a.Pinned = s.Trust, s.Pinned
		if s.State != "" || (!a.Pinned && a.Trust < pol.MinTrust) {
			continue
		}
		out = append(out, a)
	}
	if max := pol.MaxAds; max > 0 && len(out) > max {
		slices.SortStableFunc(out, func(a, b Ad) int {
			switch {
			case a.Pinned != b.Pinned:
				if a.Pinned {
					return -1
				}
				return 1
			case a.Trust != b.Trust:
				if a.Trust > b.Trust {
					return -1
				}
				return 1
			}
			return b.Created.Compare(a.Created)
		})
		out = out[:max]
		slices.SortFunc(out, func(a, b Ad) int { return compareIDs(a.ID, b.ID) })
	}
	return out
}

func compareIDs(a, b fingerprint.ID) int { return bytes.Compare(a[:], b[:]) }

// retractionLive is the condition under which a row of retractions counts: it is newer than
// the latest ad.add of the ad by the same origin (an author can add a withdrawn ad again).
const retractionLive = `ts > COALESCE((SELECT a.ts FROM ad_adds a WHERE a.ad = retractions.ad AND a.origin = retractions.origin), 0)`

// States of an ad that keep it out of the tracking set whatever its trust.
const (
	StateRetracted = "retracted" // withdrawn by its author
	StateBlocked   = "blocked"   // its author is blocked here
	StateDup       = "dup"       // a duplicate of another ad, by this node's word
)

// stateRank orders the states when several apply: the strongest is reported.
var stateRank = map[string]int{StateDup: 1, StateBlocked: 2, StateRetracted: 3}

// standing is where an ad stands with this node: its trust, the state that keeps it out
// whatever its trust ("" or a State*), and whether it is pinned.
type standing struct {
	Trust  float64
	State  string
	Pinned bool
}

// trust computes the standing of ads: the trust is the author's weight, plus each
// origin's vote times its weight, plus half the weight of each origin that confirmed the
// ad in a file; the state is the strongest reason it is out whatever its trust. Every ad
// of all has an entry. With only, the queries read the rows of these ads alone.
func (db *DB) trust(ctx context.Context, q querier, w *weigher, all []Ad, only bool) (map[fingerprint.ID]*standing, error) {
	st := make(map[fingerprint.ID]*standing, len(all))
	mark := func(ad fingerprint.ID, s string) {
		if x := st[ad]; x != nil && stateRank[s] > stateRank[x.State] {
			x.State = s
		}
	}
	add := func(ad fingerprint.ID, v float64) {
		if x := st[ad]; x != nil {
			x.Trust += v
		}
	}
	author := map[fingerprint.ID]record.Origin{}
	for _, a := range all {
		author[a.ID] = a.Author
		st[a.ID] = &standing{Trust: w.of(a.Author)}
		if w.blocked[a.Author] {
			mark(a.ID, StateBlocked)
		}
	}
	scan := func(query string, args []any, fn func(ad fingerprint.ID, o record.Origin, v int64)) error {
		return eachRowQ(ctx, q, query, args, func(rows *sql.Rows) error {
			var ad, o []byte
			var v int64
			if err := rows.Scan(&ad, &o, &v); err != nil {
				return err
			}
			var id fingerprint.ID
			var origin record.Origin
			copy(id[:], ad)
			copy(origin[:], o)
			fn(id, origin, v)
			return nil
		})
	}
	self := w.self
	// With only, the queries read the rows of these ads alone.
	in, args := "1", []any{sql.Named("self", self[:])}
	if only {
		in, args = adIn("ad"), append(args, sql.Named("ids", adIDs(all)))
	}
	steps := []func() error{
		func() error {
			return scan(`SELECT ad, origin, value FROM votes WHERE `+in, args, func(ad fingerprint.ID, o record.Origin, v int64) {
				add(ad, float64(v)*w.of(o))
			})
		},
		func() error {
			return scan(`SELECT DISTINCT ad, origin, 1 FROM peer_detections WHERE confirmed = 1 AND `+in, args,
				func(ad fingerprint.ID, o record.Origin, _ int64) { add(ad, 0.5*w.of(o)) })
		},
		func() error {
			return scan(`SELECT DISTINCT ad, :self, 1 FROM detections WHERE confirmed = 1 AND `+in, args,
				func(ad fingerprint.ID, o record.Origin, _ int64) { add(ad, 0.5*w.of(o)) })
		},
		func() error {
			return scan(`SELECT ad, origin, 0 FROM retractions WHERE `+in+` AND `+retractionLive, args, func(ad fingerprint.ID, o record.Origin, _ int64) {
				if a, ok := author[ad]; ok && (a == o || a == record.Origin{}) {
					mark(ad, StateRetracted)
				}
			})
		},
		func() error {
			return scan(`SELECT ad, origin, 0 FROM dups WHERE origin = :self AND `+in, args,
				func(ad fingerprint.ID, _ record.Origin, _ int64) { mark(ad, StateDup) })
		},
		func() error {
			return scan(`SELECT ad, ad, 0 FROM pins WHERE `+in, args, func(ad fingerprint.ID, _ record.Origin, _ int64) {
				if x := st[ad]; x != nil {
					x.Pinned = true
				}
			})
		},
	}
	for _, s := range steps {
		if err := s(); err != nil {
			return nil, fmt.Errorf("computing trust: %w", err)
		}
	}
	return st, nil
}
