package replica

import (
	"context"
	"testing"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

// syncAll runs one round of every node against its peers, in order.
func syncAll(t *testing.T, links map[*testNode][]Peer) {
	t.Helper()
	for n, peers := range links {
		sy := NewSyncer(t.Context(), n.db, nil, func(context.Context) error { return nil })
		for _, p := range peers {
			if err := sy.Round(t.Context(), p); err != nil {
				t.Fatalf("round to %s: %v", p.Name, err)
			}
		}
	}
}

// settle syncs until no node's log grows, failing if that takes too long (a loop).
func settle(t *testing.T, links map[*testNode][]Peer, nodes ...*testNode) {
	t.Helper()
	for range 10 {
		before := make([]int64, len(nodes))
		for i, n := range nodes {
			before[i] = head(t, n.db)
		}
		syncAll(t, links)
		quiet := true
		for i, n := range nodes {
			quiet = quiet && head(t, n.db) == before[i]
		}
		if quiet {
			return
		}
	}
	t.Fatal("logs still growing after 10 rounds: records are echoing")
}

// Records travel over several hops: c only knows b, b only knows a.
func TestChainRelay(t *testing.T) {
	a, b, c := newNode(t), newNode(t), newNode(t)
	ad := enroll(t, a.db, 1)
	links := map[*testNode][]Peer{b: {a.peer("a", true, false)}, c: {b.peer("b", true, false)}}
	settle(t, links, a, b, c)
	if !hasAd(t, c.db, ad) {
		t.Fatal("the ad did not reach c through b")
	}
	if head(t, c.db) != 1 {
		t.Fatalf("c holds %d records, want 1", head(t, c.db))
	}
}

// A full mesh of four nodes, each with its own ads, converges without duplicates or echoes.
func TestMeshConverges(t *testing.T) {
	nodes := []*testNode{newNode(t), newNode(t), newNode(t), newNode(t)}
	links := map[*testNode][]Peer{}
	var ids []fingerprint.ID
	for i, n := range nodes {
		for k := range 3 {
			ids = append(ids, enroll(t, n.db, int32(i*10+k)))
		}
		for j, m := range nodes {
			if i != j {
				links[n] = append(links[n], m.peer("p", true, true))
			}
		}
	}
	settle(t, links, nodes...)
	for _, n := range nodes {
		if got := head(t, n.db); got != 12 {
			t.Fatalf("a node holds %d records, want 12", got)
		}
		for _, id := range ids {
			if !hasAd(t, n.db, id) {
				t.Fatal("an ad is missing after convergence")
			}
		}
	}
}

// Peers behind NAT push to a hub that never dials out; a third node pulls from the hub.
func TestHubAndSpokes(t *testing.T) {
	hub, s1, s2, reader := newNode(t), newNode(t), newNode(t), newNode(t)
	a1, a2 := enroll(t, s1.db, 1), enroll(t, s2.db, 2)
	links := map[*testNode][]Peer{
		s1: {hub.peer("hub", false, true)}, s2: {hub.peer("hub", false, true)},
		reader: {hub.peer("hub", true, false)},
	}
	settle(t, links, hub, s1, s2, reader)
	if !hasAd(t, reader.db, a1) || !hasAd(t, reader.db, a2) {
		t.Fatal("the reader is missing ads that were pushed to the hub")
	}
	if head(t, s1.db) != 1 {
		t.Fatal("a push-only spoke received records")
	}
}
