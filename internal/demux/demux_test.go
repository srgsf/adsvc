package demux

import (
	"bytes"
	"encoding/csv"
	"math"
	"math/rand/v2"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// recSink records what a container produces.
type recSink struct {
	format     AudioFormat
	configured int
	frames     []Frame
	closed     bool
}

func (r *recSink) Configure(f AudioFormat) error {
	r.format = f
	r.configured++
	return nil
}

func (r *recSink) Frame(f Frame) error {
	f.Data = append([]byte(nil), f.Data...)
	r.frames = append(r.frames, f)
	return nil
}

func (r *recSink) Close() { r.closed = true }

// span is a byte range [from, to) of a file, as a player would request it.
type span struct{ from, to int64 }

// demux runs a container over the given ranges of data, fed in proxy-sized pieces. Each
// piece is a copy that is overwritten after Write, as the proxy reuses its buffers: a
// container that kept p would see garbage.
func demux(t *testing.T, typ string, data []byte, ranges ...span) *recSink {
	t.Helper()
	sink := &recSink{}
	c, err := New(typ, sink)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranges) == 0 {
		ranges = []span{{0, int64(len(data))}}
	}
	for _, r := range ranges {
		c.Range(r.from)
		piece := make([]byte, 64<<10)
		for p := r.from; p < r.to; p += 64 << 10 {
			end := min(p+64<<10, r.to)
			b := piece[:copy(piece, data[p:end])]
			if err := c.Write(b); err != nil {
				t.Fatalf("write at %d: %v", p, err)
			}
			for i := range b {
				b[i] = 0xA5
			}
		}
	}
	c.Close()
	return sink
}

type probePacket struct {
	pts  float64
	size int
}

// probeAudio returns ffprobe's packets of the first audio stream, with times on the
// player's timeline (the file's start time subtracted, as players do).
func probeAudio(t *testing.T, path string) []probePacket {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error", "-show_entries", "format=start_time",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	start, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	out, err = exec.Command("ffprobe", "-v", "error", "-select_streams", "a:0",
		"-show_entries", "packet=pts_time,size", "-of", "csv=p=0", path).Output()
	if err != nil {
		t.Fatalf("ffprobe: %v", err)
	}
	cr := csv.NewReader(bytes.NewReader(out))
	cr.FieldsPerRecord = -1 // some packets carry an extra side-data column
	recs, err := cr.ReadAll()
	if err != nil {
		t.Fatalf("ffprobe output: %v", err)
	}
	var pk []probePacket
	for _, r := range recs {
		if len(r) < 2 {
			continue
		}
		pts, e1 := strconv.ParseFloat(r[0], 64)
		size, e2 := strconv.Atoi(r[1])
		if e1 == nil && e2 == nil {
			pk = append(pk, probePacket{pts - start, size})
		}
	}
	return pk
}

// checkAgainstProbe requires one frame per ffprobe packet, at the same time and size.
func checkAgainstProbe(t *testing.T, frames []Frame, pk []probePacket, tol float64) {
	t.Helper()
	if len(frames) != len(pk) {
		t.Fatalf("%d frames, ffprobe has %d packets", len(frames), len(pk))
	}
	worst := 0.0
	for i, f := range frames {
		if len(f.Data) != pk[i].size {
			t.Fatalf("frame %d: %d bytes, ffprobe %d", i, len(f.Data), pk[i].size)
		}
		worst = math.Max(worst, math.Abs(f.Time.Seconds()-pk[i].pts))
	}
	if worst > tol {
		t.Fatalf("frame times differ from ffprobe by up to %.4fs", worst)
	}
}

// checkSeekSuffix requires the frames read after a seek to be the frames of a full read.
func checkSeekSuffix(t *testing.T, full, part []Frame) {
	t.Helper()
	if len(part) == 0 {
		t.Fatal("no frames after the seek")
	}
	at := map[int64]int{}
	for i, f := range full {
		at[int64(math.Round(f.Time.Seconds()*1e6))] = i
	}
	for _, f := range part {
		i, ok := at[int64(math.Round(f.Time.Seconds()*1e6))]
		if !ok || !bytes.Equal(full[i].Data, f.Data) {
			t.Fatalf("frame at %.6fs after the seek is not in the full read", f.Time.Seconds())
		}
	}
}

func TestSniff(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want string
	}{
		{"avi", append([]byte("RIFF\x00\x00\x00\x00AVI LIST"), make([]byte, 8)...), "avi"},
		{"matroska", append([]byte{0x1A, 0x45, 0xDF, 0xA3}, make([]byte, 20)...), "matroska"},
		{"mp4", append([]byte("\x00\x00\x00\x18ftypisom"), make([]byte, 16)...), "mp4"},
		{"text", bytes.Repeat([]byte("x"), 300), ""},
		{"short", []byte("RIFF"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Sniff(tt.head); got != tt.want {
				t.Errorf("Sniff = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFrameDelay(t *testing.T) {
	var got []Frame
	d := frameDelay{emit: func(f Frame) error { got = append(got, f); return nil }}
	ms := time.Millisecond
	for _, err := range []error{
		d.push(1000*ms, 0, nil, []byte{1}, []byte{2}), // a laced group of two, duration unknown
		d.push(1500*ms, 0, nil, []byte{3}),
		d.push(9000*ms, 0, nil, []byte{4}), // a jump: not a duration
		d.flush(),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	want := []Frame{{Time: 1000 * ms, Dur: 250 * ms}, {Time: 1250 * ms, Dur: 250 * ms}, {Time: 1500 * ms, Dur: 250 * ms}, {Time: 9000 * ms, Dur: 250 * ms}}
	if len(got) != len(want) {
		t.Fatalf("got %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Time != want[i].Time || got[i].Dur != want[i].Dur {
			t.Errorf("frame %d: %v %v, want %v %v", i, got[i].Time, got[i].Dur, want[i].Time, want[i].Dur)
		}
	}
}

// resync finds a marker wherever the writes split it, and only a marker check accepts.
func TestResync(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for trial := range 300 {
		data := make([]byte, 200+r.IntN(300))
		for i := range data {
			data[i] = byte(r.IntN(256))
		}
		bait := r.IntN(len(data) - 40) // the pattern without a valid marker: skipped
		copy(data[bait:], "xxxxmoof")
		data[bait+8] = 0
		at := bait + 10 + r.IntN(len(data)-bait-40)
		copy(data[at:], "SIZEmoofxxxxmfhd")
		rs := resync{pattern: []byte("moof"), lead: 4, size: 16, check: func(b []byte) (bool, bool) {
			if len(b) < 16 {
				return false, true
			}
			return string(b[12:16]) == "mfhd", false
		}}
		got, off := -1, 0
		for p := data; len(p) > 0 && got < 0; {
			n := min(len(p), 1+r.IntN(24))
			head, rest, ok := rs.find(p[:n])
			if ok {
				got = off + (n - len(rest)) - len(head)
				if len(head) > 0 && string(append(head, rest...)[:16]) != "SIZEmoofxxxxmfhd" {
					t.Fatalf("trial %d: head+rest do not start with the marker", trial)
				}
			}
			off, p = off+n, p[n:]
		}
		if got != at {
			t.Fatalf("trial %d: marker found at %d, want %d", trial, got, at)
		}
	}
}
