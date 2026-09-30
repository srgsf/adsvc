package catalog

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestReports(t *testing.T) {
	db := node(t)
	ctx := t.Context()
	add := func(user, norm string, pos int32, note string) int64 {
		t.Helper()
		id, err := db.AddReport(ctx, Report{User: user, Norm: norm, URL: "http://" + norm, PosMs: pos, Note: note})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a := add("alice", "h/a.mkv", 60_000, "")
	if again := add("alice", "h/a.mkv", 65_000, "late"); again != a {
		t.Fatalf("a report 5 s later is a new one: %d vs %d", again, a)
	}
	b := add("alice", "h/a.mkv", 600_000, "")
	c := add("bob", "h/a.mkv", 60_000, "")
	if c == a || b == a {
		t.Fatal("reports merged across users or far positions")
	}
	mine, err := db.Reports(ctx, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 2 {
		t.Fatalf("alice sees %d reports", len(mine))
	}
	all, err := db.Reports(ctx, "")
	if err != nil || len(all) != 3 {
		t.Fatalf("all: %d, %v", len(all), err)
	}
	if r, ok, _ := db.Report(ctx, a, "alice"); !ok || r.PosMs != 65_000 || r.Note != "late" {
		t.Fatalf("merged report: %+v %v", r, ok)
	}
	if _, ok, _ := db.Report(ctx, a, "bob"); ok {
		t.Fatal("bob reads alice's report")
	}
	if ok, err := db.DeleteReport(ctx, a, "bob"); err != nil || ok {
		t.Fatalf("bob deleted alice's report: %v %v", ok, err)
	}
	n, err := db.DeleteReportsIn(ctx, "alice", []string{"", "h/a.mkv"}, 50_000, 80_000)
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	if ok, err := db.DeleteReport(ctx, b, ""); err != nil || !ok {
		t.Fatalf("admin delete: %v %v", ok, err)
	}
	if left, _ := db.Reports(ctx, ""); len(left) != 1 || left[0].User != "bob" {
		t.Fatalf("left: %+v", left)
	}
}

func TestSnapshotDropsReports(t *testing.T) {
	db := node(t)
	if _, err := db.AddReport(t.Context(), Report{User: "u", Norm: "n", URL: "http://secret.example/x"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "snap.db")
	if err := db.Snapshot(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	s, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.QueryRow(`SELECT count(*) FROM reports`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("snapshot holds %d reports (%v)", n, err)
	}
}
