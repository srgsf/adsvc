package catalog

import (
	"context"
	"database/sql"
	"fmt"
	"slices"

	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/record"
)

// PeerDetection is an ad another origin found in a file.
type PeerDetection struct {
	Origin record.Origin
	Detection
}

// PeerAds returns the ads other origins found in the file known by keys, merged under the
// policy: detections of the same ad that start within 250 ms are one; one is kept if its
// ad is trusted and the weights of the origins that report it add up to FileMapMin, and
// it is confirmed if one of them confirmed it. See docs/database.md §4.
func (db *DB) PeerAds(ctx context.Context, keys ...string) ([]Detection, error) {
	type info struct{ label, typ string }
	var have map[fingerprint.ID]info
	tracked := func(id fingerprint.ID) (string, string, bool) {
		i, ok := have[id]
		return i.label, i.typ, ok
	}
	return db.peerAds(ctx, func(ctx context.Context) (Tracked, error) {
		ads, err := db.Tracking(ctx) // only trusted ads are reported
		have = make(map[fingerprint.ID]info, len(ads))
		for _, a := range ads {
			have[a.ID] = info{a.Label, a.Type}
		}
		return tracked, err
	}, keys...)
}

// Tracked says whether an ad is in the tracking set, and its label and type.
type Tracked func(fingerprint.ID) (label, typ string, ok bool)

// PeerAdsIn is PeerAds with the tracking set given by the caller (one that holds it
// already, such as the ad index), rather than computed from the whole catalogue.
func (db *DB) PeerAdsIn(ctx context.Context, tracked Tracked, keys ...string) ([]Detection, error) {
	return db.peerAds(ctx, func(context.Context) (Tracked, error) { return tracked, nil }, keys...)
}

// peerAds is PeerAds; set gives the tracking set, asked for only when there are
// detections.
func (db *DB) peerAds(ctx context.Context, set func(context.Context) (Tracked, error), keys ...string) ([]Detection, error) {
	dets, err := db.PeerDetections(ctx, keys...)
	if err != nil || len(dets) == 0 {
		return nil, err
	}
	tracked, err := set(ctx)
	if err != nil {
		return nil, err
	}
	w, err := db.weigher(ctx, db.r)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(dets, func(a, b PeerDetection) int {
		if c := compareIDs(a.Ad, b.Ad); c != 0 {
			return c
		}
		return int(a.StartMs) - int(b.StartMs)
	})
	var out []Detection
	for i := 0; i < len(dets); {
		j, sum, confirmed := i, 0.0, false
		origins := map[record.Origin]bool{}
		best := dets[i]
		for j < len(dets) && dets[j].Ad == dets[i].Ad && dets[j].StartMs-dets[i].StartMs <= 250 {
			if !origins[dets[j].Origin] {
				origins[dets[j].Origin] = true
				sum += w.of(dets[j].Origin)
			}
			confirmed = confirmed || dets[j].Confirmed
			if dets[j].Score > best.Score {
				best = dets[j]
			}
			j++
		}
		if l, t, ok := tracked(best.Ad); ok && sum >= w.pol.FileMapMin {
			d := best.Detection
			d.Label, d.Type, d.Confirmed = l, t, confirmed
			out = append(out, d)
		}
		i = j
	}
	return out, nil
}

// PeerDetections returns what other origins found in the file known by keys (as their
// key or an alias), unmerged and unfiltered; labels are not filled in.
func (db *DB) PeerDetections(ctx context.Context, keys ...string) ([]PeerDetection, error) {
	var dets []PeerDetection
	seen := map[string]bool{}
	for _, k := range keys {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		if err := db.eachRow(ctx, `SELECT d.origin, d.ad, d.start_ms, d.end_ms, d.score, d.confirmed FROM peer_detections d
			WHERE d.key = ? OR (d.key, d.origin) IN (SELECT key, origin FROM peer_aliases WHERE alias = ?)`,
			[]any{k, k}, func(rows *sql.Rows) error {
				var p PeerDetection
				var o, ad []byte
				if err := rows.Scan(&o, &ad, &p.StartMs, &p.EndMs, &p.Score, &p.Confirmed); err != nil {
					return err
				}
				copy(p.Origin[:], o)
				copy(p.Ad[:], ad)
				dets = append(dets, p)
				return nil
			}); err != nil {
			return nil, fmt.Errorf("reading peer detections: %w", err)
		}
	}
	return dets, nil
}
