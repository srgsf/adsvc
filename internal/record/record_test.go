package record

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

var now = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

func adAdd(t testing.TB) *AdAdd {
	t.Helper()
	var pts []fingerprint.Point
	for i := range 80 {
		pts = append(pts, fingerprint.Point{H: uint32(i * 7919 % (1 << fingerprint.HashBits)), T: int32(i / 6)})
	}
	b, err := fingerprint.Encode(pts)
	if err != nil {
		t.Fatal(err)
	}
	return &AdAdd{ID: fingerprint.IDOf(b), FPVersion: fingerprint.Version, DurationMs: 30000, Points: b, Label: "Brand <X> & co"}
}

func signed(t testing.TB, id *Identity, kind string, seq uint64, body any) Record {
	t.Helper()
	r, err := New(kind, seq, now.UnixMilli(), body)
	if err != nil {
		t.Fatal(err)
	}
	id.Sign(&r)
	return r
}

func TestSignVerify(t *testing.T) {
	id := NewIdentity()
	r := signed(t, id, KindAdAdd, 1, adAdd(t))
	if !r.Verify() || r.Origin != id.Origin {
		t.Fatal("a fresh record does not verify")
	}
	if !bytes.Contains(r.Body, []byte("Brand <X> & co")) {
		t.Fatalf("body HTML-escaped: %s", r.Body)
	}
	for name, tamper := range map[string]func(*Record){
		"kind":   func(r *Record) { r.Kind = KindAdLabel },
		"origin": func(r *Record) { r.Origin = NewIdentity().Origin },
		"seq":    func(r *Record) { r.Seq++ },
		"ts":     func(r *Record) { r.TS++ },
		"body":   func(r *Record) { r.Body = bytes.Replace(r.Body, []byte("Brand"), []byte("brand"), 1) },
		"sig":    func(r *Record) { r.Sig[0] ^= 1 },
	} {
		c := r
		c.Body = bytes.Clone(r.Body)
		tamper(&c)
		if c.Verify() {
			t.Errorf("tampered %s still verifies", name)
		}
		if c.Hash() == r.Hash() && name != "sig" {
			t.Errorf("tampered %s keeps the hash", name)
		}
	}
}

func TestFrames(t *testing.T) {
	id := NewIdentity()
	recs := []Record{
		signed(t, id, KindAdAdd, 1, adAdd(t)),
		signed(t, id, KindAdLabel, 2, AdLabel{Ad: adAdd(t).ID, Label: "x"}),
		signed(t, id, "future.kind", 3, map[string]int{"n": 1}),
	}
	var b []byte
	for i := range recs {
		b = AppendFrame(b, &recs[i])
	}
	br := bufio.NewReader(bytes.NewReader(b))
	for i := range recs {
		got, err := ReadFrame(br)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != recs[i].Kind || got.Seq != recs[i].Seq || !bytes.Equal(got.Body, recs[i].Body) || !got.Verify() {
			t.Fatalf("frame %d: %+v", i, got)
		}
	}
	if _, err := ReadFrame(br); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream: %v", err)
	}
	for cut := 1; cut < len(b); cut += 97 {
		br := bufio.NewReader(bytes.NewReader(b[:cut]))
		for {
			_, err := ReadFrame(br)
			if err != nil {
				if errors.Is(err, io.EOF) && !endsFrame(b, cut) {
					t.Fatalf("truncated at %d reported as a clean end", cut)
				}
				break
			}
		}
	}
}

// endsFrame reports whether cut is a frame boundary of b.
func endsFrame(b []byte, cut int) bool {
	br := bufio.NewReader(bytes.NewReader(b))
	off := 0
	for off < cut {
		r, err := ReadFrame(br)
		if err != nil {
			return false
		}
		off += len(AppendFrame(nil, &r))
	}
	return off == cut
}

func TestCheck(t *testing.T) {
	id := NewIdentity()
	ok := signed(t, id, KindAdAdd, 1, adAdd(t))
	if err := Check(&ok, now); err != nil {
		t.Fatalf("valid record: %v", err)
	}
	unknown := signed(t, id, "ad.rating", 2, map[string]any{"ad": "x", "stars": 5})
	if err := Check(&unknown, now); err != nil {
		t.Fatalf("a record of a newer kind must be accepted (stored, not materialized): %v", err)
	}
	future := ok
	future.TS = now.Add(MaxSkew + time.Minute).UnixMilli()
	id.Sign(&future)
	old := ok
	old.TS = 1000
	id.Sign(&old)
	badSig := ok
	badSig.Sig[5] ^= 1
	seq0 := ok
	seq0.Seq = 0
	id.Sign(&seq0)
	for _, c := range []struct {
		name   string
		r      Record
		reason string
	}{
		{"future", future, RejectFuture},
		{"too old", old, RejectInvalid},
		{"signature", badSig, RejectSignature},
		{"seq 0", seq0, RejectInvalid},
		{"array body", signed(t, id, KindAdLabel, 3, []int{1}), RejectInvalid},
		{"bad kind", signed(t, id, "Ad.Add", 3, map[string]int{}), RejectInvalid},
	} {
		var rej *Rejection
		if err := Check(&c.r, now); !errors.As(err, &rej) || rej.Reason != c.reason {
			t.Errorf("%s: %v, want reason %s", c.name, err, c.reason)
		}
	}
}

func TestBodies(t *testing.T) {
	id := NewIdentity()
	good := adAdd(t)
	ad := good.ID
	start := int32(1000)
	for _, c := range []struct {
		name  string
		kind  string
		body  any
		valid bool
	}{
		{"ad", KindAdAdd, good, true},
		{"ad with source", KindAdAdd, func() *AdAdd { a := *good; a.Source = &Source{Key: "ih:ab/1", StartMs: 5000, EndMs: 35000}; return &a }(), true},
		{"ad id not the hash", KindAdAdd, func() *AdAdd { a := *good; a.ID[0] ^= 1; return &a }(), false},
		{"ad of another version keeps its hash", KindAdAdd, func() *AdAdd { a := *good; a.FPVersion = 2; return &a }(), false},
		{"ad of another version", KindAdAdd, func() *AdAdd {
			a := *good
			a.FPVersion = 2
			a.ID = fingerprint.IDFor(2, a.Points)
			return &a
		}(), true},
		{"ad too long", KindAdAdd, func() *AdAdd { a := *good; a.DurationMs = MaxAdDurationMs + 1; return &a }(), false},
		{"ad with a URL as source", KindAdAdd, func() *AdAdd { a := *good; a.Source = &Source{Key: "http://x/y.mkv", EndMs: 1}; return &a }(), false},
		{"ad with control characters", KindAdAdd, func() *AdAdd { a := *good; a.Label = "a\nb"; return &a }(), false},
		{"label", KindAdLabel, AdLabel{Ad: ad, Label: strings.Repeat("é", MaxLabel)}, true},
		{"intro label", KindAdLabel, AdLabel{Ad: ad, Label: "x", Type: "intro"}, true},
		{"unknown type", KindAdLabel, AdLabel{Ad: ad, Label: "x", Type: "credits"}, false},
		{"intro ad", KindAdAdd, func() *AdAdd { a := *good; a.Type = "intro"; return &a }(), true},
		{"ad with unknown type", KindAdAdd, func() *AdAdd { a := *good; a.Type = "outro"; return &a }(), false},
		{"long label", KindAdLabel, AdLabel{Ad: ad, Label: strings.Repeat("é", MaxLabel+1)}, false},
		{"vote", KindAdVote, AdVote{Ad: ad, Value: -1, Reason: ReasonNotAd, Key: "c:12", StartMs: &start}, true},
		{"vote of 2", KindAdVote, AdVote{Ad: ad, Value: 2, Reason: ReasonGood}, false},
		{"vote without reason", KindAdVote, AdVote{Ad: ad, Value: 1}, false},
		{"dup", KindAdDup, AdDup{Ad: ad, Canonical: fingerprint.ID{1}}, true},
		{"dup of itself", KindAdDup, AdDup{Ad: ad, Canonical: ad}, false},
		{"retract", KindAdRetract, AdRetract{Ad: ad}, true},
		{"retract nothing", KindAdRetract, AdRetract{}, false},
		{"file map", KindFileMap, FileMap{Key: "ih:ab/1", Aliases: []string{"c:12"}, FPVersion: 1, Size: 10, DurationMs: 60000,
			Analyzed: [][2]int32{{0, 30000}}, Ads: []Detection{{Ad: ad, StartMs: -500, EndMs: 29500, Score: 50, Confirmed: true}}}, true},
		{"file map by URL", KindFileMap, FileMap{Key: "https://ts.example.com/stream/x.mkv", FPVersion: 1}, false},
		{"file map with a name as alias", KindFileMap, FileMap{Key: "c:1", Aliases: []string{"Episode 3.mkv"}, FPVersion: 1}, false},
		{"file map, backwards range", KindFileMap, FileMap{Key: "c:1", FPVersion: 1, Ads: []Detection{{Ad: ad, StartMs: 10, EndMs: 5}}}, false},
		{"unknown fields are ignored", KindAdLabel, map[string]any{"ad": ad, "label": "x", "added_in_v9": true}, true},
	} {
		r := signed(t, id, c.kind, 1, c.body)
		err := Check(&r, now)
		if (err == nil) != c.valid {
			t.Errorf("%s: %v, want valid=%v", c.name, err, c.valid)
		}
	}
}

func TestIdentityFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "identity.key")
	a, err := LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadIdentity(path)
	if err != nil || b.Origin != a.Origin {
		t.Fatalf("reloaded identity: %v, %v", b, err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("identity file mode %v", fi.Mode().Perm())
	}
	r := signed(t, a, KindAdRetract, 1, AdRetract{Ad: fingerprint.ID{1}})
	if !r.Verify() {
		t.Fatal("a record signed by a loaded identity does not verify")
	}
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadIdentity(path); err == nil {
		t.Fatal("a corrupt identity file was accepted")
	}
	o, err := ParseOrigin(a.Origin.String())
	if err != nil || o != a.Origin {
		t.Fatalf("origin round trip: %v", err)
	}
}

func TestClock(t *testing.T) {
	wall := now
	c := NewClock(0)
	c.now = func() time.Time { return wall }
	a, b := c.Next(), c.Next()
	if a != now.UnixMilli() || b != a+1 {
		t.Fatalf("Next: %d, %d", a, b)
	}
	c.Observe(b + 5000)
	if n := c.Next(); n != b+5001 {
		t.Fatalf("after observing a later time: %d", n)
	}
	c.Observe(now.Add(time.Hour).UnixMilli()) // too far ahead: ignored
	if n := c.Next(); n != b+5002 {
		t.Fatalf("a far-future time moved the clock: %d", n)
	}
	wall = wall.Add(-time.Minute) // the wall clock steps back
	if n := c.Next(); n != b+5003 {
		t.Fatalf("the clock went backwards: %d", n)
	}
}

// Any bytes: ParseFrame never panics, and what it accepts frames back to the same bytes.
func FuzzParseFrame(f *testing.F) {
	id := NewIdentity()
	r := signed(f, id, KindAdAdd, 7, adAdd(f))
	fr := AppendFrame(nil, &r)
	_, n := binaryUvarintLen(fr)
	f.Add(fr[n:])
	f.Add([]byte{1, 'a'})
	f.Fuzz(func(t *testing.T, p []byte) {
		r, err := ParseFrame(p)
		if err != nil {
			return
		}
		again := AppendFrame(nil, &r)
		_, n := binaryUvarintLen(again)
		if !bytes.Equal(again[n:], p) {
			t.Fatalf("re-framed differently")
		}
		_ = Check(&r, now)
	})
}

// Any body: decoding a signed record of every kind never panics.
func FuzzBodies(f *testing.F) {
	id := NewIdentity()
	f.Add([]byte(`{"ad":"77e44851-967c-f930-6ecf-67fa7b868ffd","label":"x"}`))
	f.Add([]byte(`{"key":"c:1","fp_version":1,"ads":[{"ad":"77e44851-967c-f930-6ecf-67fa7b868ffd","start_ms":1,"end_ms":2}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		for _, k := range []string{KindAdAdd, KindAdLabel, KindAdVote, KindAdDup, KindAdRetract, KindFileMap} {
			r := Record{Kind: k, Seq: 1, TS: now.UnixMilli(), Body: body}
			id.Sign(&r)
			_ = Check(&r, now)
		}
	})
}

func binaryUvarintLen(b []byte) (uint64, int) {
	var v uint64
	for i, c := range b {
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}
