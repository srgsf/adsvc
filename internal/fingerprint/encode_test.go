package fingerprint

import (
	"bytes"
	"cmp"
	"encoding/json"
	"math/rand/v2"
	"slices"
	"testing"
)

func randomPoints(r *rand.Rand, n int) []Point {
	pts := make([]Point, n)
	for i := range pts {
		pts[i] = Point{H: r.Uint32() >> (32 - HashBits), T: int32(r.IntN(2000))}
	}
	return pts
}

func canonical(pts []Point) []Point {
	s := slices.Clone(pts)
	slices.SortFunc(s, func(a, b Point) int { return cmp.Or(cmp.Compare(a.T, b.T), cmp.Compare(a.H, b.H)) })
	return s
}

func TestEncodeRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for _, n := range []int{0, 1, 6, 5000} {
		pts := randomPoints(r, n)
		b, err := Encode(pts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(b)
		if err != nil {
			t.Fatalf("n=%d: %v", n, err)
		}
		if !slices.Equal(got, canonical(pts)) {
			t.Fatalf("n=%d: round trip differs", n)
		}
	}
}

// Real fingerprints have about fanOut landmarks per anchor frame.
func TestEncodeSize(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	pts := randomPoints(r, 6000)
	for i := range pts {
		pts[i].T = int32(i / fanOut)
	}
	b, err := Encode(pts)
	if err != nil {
		t.Fatal(err)
	}
	if per := float64(len(b)) / float64(len(pts)); per > 3.4 {
		t.Errorf("%.2f bytes per landmark", per)
	}
}

// The order landmarks come in does not change the encoding or the ID.
func TestIDIsCanonical(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))
	pts := randomPoints(r, 300)
	a, _ := Encode(pts)
	r.Shuffle(len(pts), func(i, j int) { pts[i], pts[j] = pts[j], pts[i] })
	b, _ := Encode(pts)
	if !bytes.Equal(a, b) || IDOf(a) != IDOf(b) {
		t.Fatal("encoding depends on landmark order")
	}
	pts[0].H ^= 1
	c, _ := Encode(pts)
	if IDOf(c) == IDOf(a) {
		t.Fatal("different landmarks, same ID")
	}
}

func TestEncodeRejects(t *testing.T) {
	if _, err := Encode([]Point{{H: 1, T: -1}}); err == nil {
		t.Error("negative frame accepted")
	}
	if _, err := Encode([]Point{{H: 1 << HashBits, T: 0}}); err == nil {
		t.Error("hash wider than HashBits accepted")
	}
}

func TestDecodeRejectsNonCanonical(t *testing.T) {
	for name, b := range map[string][]byte{
		"truncated hash":     {0, 1, 1, 2},
		"zero count":         {0, 0},
		"repeated frame":     {0, 1, 1, 0, 0, 0, 1, 2, 0, 0},
		"descending hashes":  {0, 2, 2, 0, 0, 1, 0, 0},
		"truncated varint":   {0x80},
		"trailing count":     {5},
		"frame out of range": {0xff, 0xff, 0xff, 0xff, 0x0f, 1, 1, 0, 0},
	} {
		if _, err := Decode(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestIDText(t *testing.T) {
	id := IDOf([]byte{1, 2, 3})
	s := id.String()
	if len(s) != 36 || s[8] != '-' || s[:8] != id.Short() {
		t.Fatalf("String %q, Short %q", s, id.Short())
	}
	for _, in := range []string{s, s[:8] + s[9:13] + s[14:18] + s[19:23] + s[24:]} {
		got, err := ParseID(in)
		if err != nil || got != id {
			t.Fatalf("ParseID(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "abc", s + "0", "zz" + s[2:]} {
		if _, err := ParseID(bad); err == nil {
			t.Errorf("ParseID(%q) accepted", bad)
		}
	}
	j, err := json.Marshal(struct{ ID ID }{id})
	if err != nil || !bytes.Contains(j, []byte(`"`+s+`"`)) {
		t.Fatalf("JSON %s, %v", j, err)
	}
	var back struct{ ID ID }
	if err := json.Unmarshal(j, &back); err != nil || back.ID != id {
		t.Fatalf("JSON round trip: %v, %v", back.ID, err)
	}
}

// Decode never panics, and whatever it accepts encodes back to the same bytes.
func FuzzDecode(f *testing.F) {
	b, _ := Encode(randomPoints(rand.New(rand.NewPCG(5, 6)), 40))
	f.Add(b)
	f.Add([]byte{0, 1, 1, 2, 3})
	f.Fuzz(func(t *testing.T, b []byte) {
		pts, err := Decode(b)
		if err != nil {
			return
		}
		again, err := Encode(pts)
		if err != nil || !bytes.Equal(again, b) {
			t.Fatalf("accepted a non-canonical encoding: %x -> %x (%v)", b, again, err)
		}
	})
}
