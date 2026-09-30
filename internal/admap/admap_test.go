package admap

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/fingerprint"
)

func TestStoreAlias(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalogue.db")
	db, err := catalog.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	s := New(db)
	ad := fingerprint.IDOf([]byte("ad"))
	const ms = time.Millisecond
	if err := s.Put(t.Context(), &FileMap{Key: "ih:abc/1", Aliases: []string{"c:deadbeef"}, Duration: 2400500 * ms,
		Analyzed: detect.Intervals{{0, 30250 * ms}}, Ads: []detect.Detection{{AdID: ad, Start: 10000*ms + 600*time.Microsecond, End: 20 * time.Second, Score: 30}}}); err != nil {
		t.Fatal(err)
	}
	if m, err := s.Lookup(t.Context(), "c:deadbeef"); err != nil || m == nil || m.Key != "ih:abc/1" {
		t.Fatalf("alias lookup: %+v, %v", m, err)
	}
	// the same file streamed without an infohash finds its map through the content key
	m, err := s.Lookup(t.Context(), "nope", "c:deadbeef")
	if err != nil || m == nil || len(m.Ads) != 1 {
		t.Fatalf("content key lookup: %+v, %v", m, err)
	}
	// times go through integer milliseconds
	if d := m.Ads[0]; d.AdID != ad || d.Start != 10001*ms || d.End != 20*time.Second || m.Duration != 2400500*ms || m.Analyzed[0] != [2]time.Duration{0, 30250 * ms} {
		t.Fatalf("round trip: %+v", m)
	}
	db.Close()

	db, err = catalog.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s = New(db)
	if m, err := s.Lookup(t.Context(), "c:deadbeef"); err != nil || m == nil || len(m.Ads) != 1 {
		t.Fatalf("reload: %+v, %v", m, err)
	}
	if err := s.ResetAnalyzed(t.Context()); err != nil {
		t.Fatal(err)
	}
	if all, err := s.List(t.Context()); err != nil || len(all) != 1 || len(all[0].Analyzed) != 0 || len(all[0].Ads) != 1 {
		t.Fatalf("after ResetAnalyzed: %+v, %v", all, err)
	}
}

// Ranges analysed with other fingerprint parameters do not count.
func TestOtherFingerprintVersion(t *testing.T) {
	db, err := catalog.Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.PutFileMap(t.Context(), &catalog.FileMap{Key: "c:1", FPVersion: fingerprint.Version + 1,
		Analyzed: [][2]int32{{0, 60_000}}}); err != nil {
		t.Fatal(err)
	}
	m, err := New(db).Lookup(t.Context(), "c:1")
	if err != nil || m == nil || len(m.Analyzed) != 0 {
		t.Fatalf("lookup: %+v, %v", m, err)
	}
}
