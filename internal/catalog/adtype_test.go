package catalog

import (
	"testing"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/fingerprint"
)

func typeOfAd(t *testing.T, db *DB, id fingerprint.ID) string {
	t.Helper()
	all, err := db.Ads(t.Context(), fingerprint.Version)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range all {
		if a.ID == id {
			return a.Type
		}
	}
	return "<missing>"
}

// An ad's type travels in its ad.add, and a label record may change it: this node's own
// word wins here, a label that names no type leaves it, and the type filters the list.
func TestAdType(t *testing.T) {
	a, b := node(t), node(t)
	plain := enroll(t, a, 1, "plain")
	if got := typeOfAd(t, a, plain); got != adtype.Ad {
		t.Fatalf("default type %q", got)
	}
	// An ad relabelled as an intro.
	intro := enroll(t, a, 2, "opening")
	if err := a.Label(t.Context(), intro, "opening", adtype.Intro); err != nil {
		t.Fatal(err)
	}
	if got := typeOfAd(t, a, intro); got != adtype.Intro {
		t.Fatalf("type after label: %q", got)
	}
	if err := a.Label(t.Context(), intro, "renamed", ""); err != nil {
		t.Fatal(err)
	}
	if got := typeOfAd(t, a, intro); got != adtype.Intro {
		t.Fatalf("a label without a type changed it to %q", got)
	}
	if err := a.Label(t.Context(), intro, "x", "credits"); err == nil {
		t.Fatal("an unknown type was accepted")
	}

	// Replicated: b sees the same, and b's own word wins on b.
	ingest(t, b, a, records(t, a))
	if got := typeOfAd(t, b, intro); got != adtype.Intro {
		t.Fatalf("type on the peer: %q", got)
	}
	if err := b.Label(t.Context(), intro, "mine", adtype.Ad); err != nil {
		t.Fatal(err)
	}
	if got := typeOfAd(t, b, intro); got != adtype.Ad {
		t.Fatalf("this node's type must win here: %q", got)
	}
	if err := a.Label(t.Context(), intro, "later", adtype.Intro); err != nil {
		t.Fatal(err)
	}
	ingest(t, b, a, records(t, a))
	if got := typeOfAd(t, b, intro); got != adtype.Ad {
		t.Fatalf("a peer's later type beat this node's: %q", got)
	}

	rows, total, err := a.AdPage(t.Context(), AdQuery{Type: adtype.Intro})
	if err != nil || total != 1 || len(rows) != 1 || rows[0].ID != intro {
		t.Fatalf("intro filter: %d rows, total %d, %v", len(rows), total, err)
	}
}

// A file map's detections carry the type of their ad.
func TestDetectionType(t *testing.T) {
	a := node(t)
	id := enroll(t, a, 3, "opening")
	if err := a.Label(t.Context(), id, "opening", adtype.Intro); err != nil {
		t.Fatal(err)
	}
	if err := a.PublishFileMap(t.Context(), &FileMap{Key: "c:9", FPVersion: fingerprint.Version, Size: 1,
		Detections: []Detection{{Ad: id, StartMs: 1000, EndMs: 31000, Confirmed: true}}}); err != nil {
		t.Fatal(err)
	}
	maps, err := a.FileMaps(t.Context())
	if err != nil || len(maps) != 1 || len(maps[0].Detections) != 1 || maps[0].Detections[0].Type != adtype.Intro {
		t.Fatalf("maps %+v, %v", maps, err)
	}
}
