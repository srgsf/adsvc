package catalog

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

func memDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file::memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	return db
}

func version(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMigrationsInVersionOrder(t *testing.T) {
	fsys := fstest.MapFS{
		"10_ten.sql": {Data: []byte(`INSERT INTO log VALUES (10);`)},
		"2_two.sql":  {Data: []byte(`INSERT INTO log VALUES (2);`)},
		"1_one.sql":  {Data: []byte(`CREATE TABLE log (v INTEGER);`)},
	}
	for v := 3; v <= 9; v++ {
		fsys[string(rune('0'+v))+"_x.sql"] = &fstest.MapFile{Data: []byte(`SELECT 1;`)}
	}
	db := memDB(t)
	if err := migrate(t.Context(), db, fsys); err != nil {
		t.Fatal(err)
	}
	var got []int
	rows, err := db.Query(`SELECT v FROM log ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		got = append(got, v)
	}
	rows.Close()
	if !slices.Equal(got, []int{2, 10}) || version(t, db) != 10 {
		t.Fatalf("applied %v up to version %d; want [2 10] up to 10", got, version(t, db))
	}
	if err := migrate(t.Context(), db, fsys); err != nil { // idempotent
		t.Fatal(err)
	}
}

func TestMigrationErrors(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"gap":       {"1_a.sql": {Data: []byte(`SELECT 1;`)}, "3_c.sql": {Data: []byte(`SELECT 1;`)}},
		"duplicate": {"1_a.sql": {Data: []byte(`SELECT 1;`)}, "01_b.sql": {Data: []byte(`SELECT 1;`)}},
		"bad name":  {"one.sql": {Data: []byte(`SELECT 1;`)}},
	} {
		if err := migrate(t.Context(), memDB(t), fsys); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	db := memDB(t)
	fsys := fstest.MapFS{
		"1_ok.sql":  {Data: []byte(`CREATE TABLE a (v INTEGER);`)},
		"2_bad.sql": {Data: []byte(`CREATE TABLE b (v INTEGER); INSERT INTO nowhere VALUES (1);`)},
	}
	if err := migrate(t.Context(), db, fsys); err == nil || !strings.Contains(err.Error(), "0002_bad") {
		t.Fatalf("error %v, want one naming 0002_bad", err)
	}
	if version(t, db) != 1 {
		t.Fatalf("version %d after a failed migration 2", version(t, db))
	}
	if _, err := db.Exec(`SELECT * FROM b`); err == nil {
		t.Fatal("table b of the failed migration exists")
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	db := memDB(t)
	fsys := fstest.MapFS{"1_a.sql": {Data: []byte(`SELECT 1;`)}, "2_b.sql": {Data: []byte(`SELECT 1;`)}}
	if err := migrate(t.Context(), db, fsys); err != nil {
		t.Fatal(err)
	}
	delete(fsys, "2_b.sql")
	if err := migrate(t.Context(), db, fsys); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("error %v, want ErrNewerSchema", err)
	}
}

func openFile(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func testAd(t *testing.T, label string, seed int32) (Ad, []byte) {
	t.Helper()
	pts := []fingerprint.Point{{H: 7, T: seed}, {H: 9, T: seed + 3}}
	b, err := fingerprint.Encode(pts)
	if err != nil {
		t.Fatal(err)
	}
	return Ad{ID: fingerprint.IDOf(b), FPVersion: fingerprint.Version, Label: label, DurationMs: 30_000, NPoints: len(pts),
		Created: time.UnixMilli(1_700_000_000_000)}, b
}

func TestAds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalogue.db")
	db := openFile(t, path)
	ctx := t.Context()
	a, pa := testAd(t, "a", 1)
	b, pb := testAd(t, "b", 2)
	b.Source = &Source{Key: "ih:abc/1", StartMs: 10_000, EndMs: 40_000}
	for _, c := range []struct {
		ad  Ad
		pts []byte
	}{{a, pa}, {b, pb}} {
		if added, err := db.PutAd(ctx, c.ad, c.pts); err != nil || !added {
			t.Fatalf("PutAd %s: %v, %v", c.ad.Label, added, err)
		}
	}
	if added, err := db.PutAd(ctx, a, pa); err != nil || added {
		t.Fatalf("PutAd of an existing ad: added %v, %v", added, err)
	}
	db.Close()

	db = openFile(t, path) // persisted
	ads, err := db.Ads(ctx, fingerprint.Version)
	if err != nil || len(ads) != 2 {
		t.Fatalf("Ads: %d, %v", len(ads), err)
	}
	want := map[fingerprint.ID]Ad{a.ID: a, b.ID: b}
	for _, got := range ads {
		w := want[got.ID]
		if got.Label != w.Label || !got.Created.Equal(w.Created) || got.NPoints != 2 ||
			(w.Source == nil) != (got.Source == nil) || (w.Source != nil && *got.Source != *w.Source) {
			t.Errorf("ad %s: %+v, want %+v", got.ID, got, w)
		}
	}
	if !slices.IsSortedFunc(ads, func(x, y Ad) int { return strings.Compare(string(x.ID[:]), string(y.ID[:])) }) {
		t.Error("Ads not ordered by id")
	}
	var seen []fingerprint.ID
	if err := db.EachPoints(ctx, fingerprint.Version, func(id fingerprint.ID, points []byte) error {
		if fingerprint.IDOf(points) != id {
			t.Errorf("points of %s do not hash to its id", id)
		}
		seen = append(seen, id)
		return nil
	}); err != nil || len(seen) != 2 {
		t.Fatalf("EachPoints: %v, %v", seen, err)
	}
	if other, _ := db.Ads(ctx, fingerprint.Version+1); len(other) != 0 {
		t.Errorf("ads of another fingerprint version: %v", other)
	}
	if ok, err := db.deleteAd(ctx, a.ID); err != nil || !ok {
		t.Fatalf("DeleteAd: %v, %v", ok, err)
	}
	if ok, _ := db.deleteAd(ctx, a.ID); ok {
		t.Fatal("DeleteAd of a missing ad reported true")
	}
}

func TestFileMaps(t *testing.T) {
	db := openFile(t, filepath.Join(t.TempDir(), "catalogue.db"))
	ctx := t.Context()
	a, pa := testAd(t, "brand", 1)
	if _, err := db.PutAd(ctx, a, pa); err != nil {
		t.Fatal(err)
	}
	gone := fingerprint.IDOf([]byte("not stored"))
	m := &FileMap{Key: "session-less", Aliases: []string{"id:x"}, FPVersion: 1, Size: 100, DurationMs: 60_000,
		Analyzed: [][2]int32{{0, 30_000}},
		Detections: []Detection{
			{Ad: a.ID, StartMs: 10_000, EndMs: 40_000, Score: 50, Confirmed: true},
			{Ad: gone, StartMs: 45_000, EndMs: 50_000, Score: 22},
		}}
	if err := db.PutFileMap(ctx, m); err != nil {
		t.Fatal(err)
	}
	got, err := db.FileMap(ctx, "nope", "id:x")
	if err != nil || got == nil {
		t.Fatalf("lookup by alias: %v, %v", got, err)
	}
	if got.Key != m.Key || !slices.Equal(got.Aliases, []string{"id:x"}) || len(got.Detections) != 2 ||
		got.Detections[0].Label != "brand" || !got.Detections[0].Confirmed || got.Detections[1].Label != "" ||
		!slices.Equal(got.Analyzed, m.Analyzed) {
		t.Fatalf("got %+v", got)
	}

	// Re-filed under a content key found later: the old key becomes an alias.
	m2 := &FileMap{Key: "c:123", Aliases: []string{"session-less"}, FPVersion: 1, Size: 100, DurationMs: 60_000}
	if err := db.PutFileMap(ctx, m2); err != nil {
		t.Fatal(err)
	}
	got, err = db.FileMap(ctx, "id:x")
	if err != nil || got == nil || got.Key != "c:123" || !slices.Equal(got.Aliases, []string{"id:x", "session-less"}) {
		t.Fatalf("after re-filing: %+v, %v", got, err)
	}
	if all, _ := db.FileMaps(ctx); len(all) != 1 {
		t.Fatalf("%d maps after re-filing, want 1", len(all))
	}

	if err := db.PutFileMap(ctx, &FileMap{Key: "c:123", FPVersion: 1, Analyzed: [][2]int32{{0, 60_000}},
		Detections: []Detection{{Ad: a.ID, StartMs: 10_000, EndMs: 40_000, Score: 60}}}); err != nil {
		t.Fatal(err)
	}
	// Stored again under the same key (updated in place): its aliases stay, detections
	// are replaced, and a new alias is added.
	got, _ = db.FileMap(ctx, "c:123")
	if !slices.Equal(got.Aliases, []string{"id:x", "session-less"}) || len(got.Detections) != 1 || got.Detections[0].Score != 60 {
		t.Fatalf("stored again: %+v", got)
	}
	if err := db.PutFileMap(ctx, &FileMap{Key: "c:123", Aliases: []string{"ih:a/1", "id:x"}, FPVersion: 1,
		Analyzed: [][2]int32{{0, 60_000}}, Detections: []Detection{{Ad: a.ID, StartMs: 10_000, EndMs: 40_000, Score: 60}}}); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.FileMap(ctx, "ih:a/1"); got == nil || got.Key != "c:123" || len(got.Aliases) != 3 {
		t.Fatalf("new alias of a stored map: %+v", got)
	}
	if err := db.ResetAnalyzed(ctx); err != nil {
		t.Fatal(err)
	}
	got, _ = db.FileMap(ctx, "c:123")
	if len(got.Analyzed) != 0 || len(got.Detections) != 1 {
		t.Fatalf("after ResetAnalyzed: %+v", got)
	}
	if _, err := db.deleteAd(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = db.FileMap(ctx, "c:123"); len(got.Detections) != 0 {
		t.Fatalf("detections of a deleted ad kept: %+v", got.Detections)
	}
	if none, err := db.FileMap(ctx, "unknown"); none != nil || err != nil {
		t.Fatalf("unknown key: %v, %v", none, err)
	}
}

func TestMeta(t *testing.T) {
	db, err := Open(t.Context(), "") // in memory
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, ok, err := db.Meta(t.Context(), "k"); ok || err != nil {
		t.Fatalf("missing key: %v, %v", ok, err)
	}
	for _, v := range []string{"1", "2"} {
		if err := db.SetMeta(t.Context(), "k", v); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok, err := db.Meta(t.Context(), "k"); v != "2" || !ok || err != nil {
		t.Fatalf("Meta = %q, %v, %v", v, ok, err)
	}
}

// Reads run on their own connections while the writer is busy (run with -race).
func TestReadsDuringWrites(t *testing.T) {
	db := openFile(t, filepath.Join(t.TempDir(), "catalogue.db"))
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 200 {
			a, pa := testAd(t, "x", int32(i))
			if _, err := db.PutAd(ctx, a, pa); err != nil {
				t.Error(err)
				return
			}
		}
	})
	for range 3 {
		wg.Go(func() {
			for range 100 {
				if _, err := db.Ads(ctx, fingerprint.Version); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if ads, _ := db.Ads(ctx, fingerprint.Version); len(ads) != 200 {
		t.Fatalf("%d ads, want 200", len(ads))
	}
}

// A snapshot of a catalogue on disk is written from a reader connection.
func TestSnapshotFromFile(t *testing.T) {
	dir := t.TempDir()
	db := openFile(t, filepath.Join(dir, "catalogue.db"))
	a, pa := testAd(t, "a", 1)
	if _, err := db.PutAd(t.Context(), a, pa); err != nil {
		t.Fatal(err)
	}
	if err := db.Snapshot(t.Context(), filepath.Join(dir, "snap.db")); err != nil {
		t.Fatal(err)
	}
}
