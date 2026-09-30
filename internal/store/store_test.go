package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/catalog"
	"github.com/srgsf/adsvc/internal/fingerprint"
)

func putAd(t *testing.T, db *catalog.DB, seed uint32) fingerprint.ID {
	t.Helper()
	var pts []fingerprint.Point
	for i := range 80 {
		pts = append(pts, fingerprint.Point{H: seed<<8 | uint32(i), T: int32(i)})
	}
	b, err := fingerprint.Encode(pts)
	if err != nil {
		t.Fatal(err)
	}
	id := fingerprint.IDOf(b)
	if _, err := db.PutAd(t.Context(), catalog.Ad{ID: id, FPVersion: fingerprint.Version, Label: "x", DurationMs: 3000,
		NPoints: len(pts), Created: time.Now()}, b); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOpen(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	id := putAd(t, st.DB, 1)
	st.Close()
	for _, n := range []string{"catalogue.db", "tracking.csr"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	st, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Lib.Get(id) == nil {
		t.Fatal("ad not in the index after reopening")
	}
}

// An ad enrolled while sharing is off stays out of the log across a restart: reopening
// the store publishes nothing before the node's sharing policy is applied again.
func TestRestartKeepsUnsharedAds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	st, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB.SetShare(t.Context(), catalog.Share{}); err != nil {
		t.Fatal(err)
	}
	var pts []fingerprint.Point
	for i := range 80 {
		pts = append(pts, fingerprint.Point{H: 5<<8 | uint32(i), T: int32(i)})
	}
	b, err := fingerprint.Encode(pts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.AddAd(t.Context(), catalog.Ad{ID: fingerprint.IDOf(b), FPVersion: fingerprint.Version, Label: "local",
		DurationMs: 3000, NPoints: len(pts), Created: time.Now()}, b); err != nil {
		t.Fatal(err)
	}
	st.Close()

	st, err = Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.DB.SetShare(t.Context(), catalog.Share{}); err != nil {
		t.Fatal(err)
	}
	if head, err := st.DB.Head(t.Context()); err != nil || head != 0 {
		t.Fatalf("log head after a restart with sharing off: %d, %v", head, err)
	}
	if err := st.DB.SetShare(t.Context(), catalog.ShareAll); err != nil {
		t.Fatal(err)
	}
	if head, err := st.DB.Head(t.Context()); err != nil || head != 1 {
		t.Fatalf("log head once ads are shared: %d, %v", head, err)
	}
}

// Copy shares the ads but nothing written to it.
func TestCopy(t *testing.T) {
	st, err := Open(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	id := putAd(t, st.DB, 2)
	cp, err := Copy(t.Context(), st.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer cp.Close()
	if cp.Lib.Get(id) == nil {
		t.Fatal("ad not copied")
	}
	if err := cp.Maps.Put(t.Context(), &admap.FileMap{Key: "c:9"}); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.Maps.Lookup(t.Context(), "c:9"); m != nil {
		t.Fatal("a map stored in the copy reached the original")
	}
}
