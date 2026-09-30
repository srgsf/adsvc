package avi

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// writeCBRAVI muxes a raw MP3/AC-3 elementary stream into an audio-only AVI the way
// VirtualDub/mencoder do for CBR: dwScale=1, dwRate=bytes/s, dwSampleSize=1, and chunks of
// arbitrary size that split frames.
func writeCBRAVI(es []byte, tag uint16, ch, sr, byteRate, chunk int, junkEvery int) []byte {
	w := func(b *bytes.Buffer, v any) { _ = binary.Write(b, binary.LittleEndian, v) } // a bytes.Buffer never fails
	var strh, strf, avih bytes.Buffer
	w(&avih, [14]uint32{0, uint32(byteRate), 0, 0x10, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0})
	w(&strh, []byte("auds"))
	w(&strh, [4]uint32{0, 0, 0, 0}) // handler, flags, prio/lang, initial frames
	w(&strh, [8]uint32{1, uint32(byteRate), 0, uint32(len(es)), 0, 0, 1, 0})
	w(&strh, [2]uint32{0, 0})
	w(&strf, []uint16{tag, uint16(ch)})
	w(&strf, []uint32{uint32(sr), uint32(byteRate)})
	w(&strf, []uint16{1, 0, 0})
	chunkf := func(b *bytes.Buffer, id string, body []byte) {
		b.WriteString(id)
		w(b, uint32(len(body)))
		b.Write(body)
		if len(body)&1 == 1 {
			b.WriteByte(0)
		}
	}
	list := func(typ string, body []byte) []byte {
		var b bytes.Buffer
		b.WriteString("LIST")
		w(&b, uint32(len(body)+4))
		b.WriteString(typ)
		b.Write(body)
		return b.Bytes()
	}
	var strl, hdrl bytes.Buffer
	chunkf(&strl, "strh", strh.Bytes())
	chunkf(&strl, "strf", strf.Bytes())
	chunkf(&hdrl, "avih", avih.Bytes())
	hdrl.Write(list("strl", strl.Bytes()))

	var movi, idx bytes.Buffer
	n := 0
	for p := 0; p < len(es); p += chunk {
		end := min(p+chunk, len(es))
		if junkEvery > 0 && n%junkEvery == 0 {
			chunkf(&movi, "JUNK", make([]byte, 30))
		}
		idx.WriteString("00wb")
		w(&idx, []uint32{0x10, uint32(movi.Len() + 4), uint32(end - p)}) // offset relative to "movi"
		chunkf(&movi, "00wb", es[p:end])
		n++
	}
	var body bytes.Buffer
	body.WriteString("AVI ")
	body.Write(list("hdrl", hdrl.Bytes()))
	body.Write(list("movi", movi.Bytes()))
	chunkf(&body, "idx1", idx.Bytes())
	var f bytes.Buffer
	f.WriteString("RIFF")
	w(&f, uint32(body.Len()))
	f.Write(body.Bytes())
	return f.Bytes()
}

func TestCBRSplitFrames(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg missing")
	}
	dir := t.TempDir()
	cases := []struct {
		name, fmt, codec string
		tag              uint16
		sr, ch, kbps     int
		samplesPerFrame  int
	}{
		{"mp3_48k_128", "mp3", "libmp3lame", 0x55, 48000, 2, 128, 1152},
		{"mp3_44k_192", "mp3", "libmp3lame", 0x55, 44100, 2, 192, 1152}, // padded frames
		{"ac3_48k_384", "ac3", "ac3", 0x2000, 48000, 6, 384, 1536},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			es := filepath.Join(dir, c.name+"."+c.fmt)
			args := []string{"-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=f=440:r=" + itoa(c.sr),
				"-t", "60", "-ac", itoa(c.ch), "-c:a", c.codec, "-b:a", itoa(c.kbps) + "k", "-f", c.fmt}
			if c.codec == "libmp3lame" {
				args = append(args, "-write_xing", "0", "-id3v2_version", "0")
			}
			if b, err := exec.Command("ffmpeg", append(args, es)...).CombinedOutput(); err != nil {
				t.Skipf("%v %s", err, b)
			}
			raw, _ := os.ReadFile(es)
			data := writeCBRAVI(raw, c.tag, c.ch, c.sr, c.kbps*1000/8, 1000, 7)
			path := filepath.Join(dir, c.name+".avi")
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}

			h, a, ix := loadIndex(t, data, false)
			full, _ := extract(h, a, ix, data, 0)
			if len(full) < 100 {
				t.Fatalf("only %d frames", len(full))
			}
			// ground truth for CBR: frame i starts at i*samplesPerFrame/sr
			maxErr := 0.0
			for i, f := range full {
				maxErr = math.Max(maxErr, math.Abs(f.t-float64(i*c.samplesPerFrame)/float64(c.sr)))
			}
			var worstSeek float64
			for _, pct := range []int64{13, 37, 61, 88} {
				part, _ := extract(h, a, ix, data, int64(len(data))*pct/100)
				// find same frame in full read by nearest time; error = distance
				j := 0
				for _, f := range part {
					for j < len(full)-1 && full[j+1].t <= f.t+1e-9 {
						j++
					}
					d := math.Min(math.Abs(f.t-full[j].t), math.Abs(f.t-full[min(j+1, len(full)-1)].t))
					worstSeek = math.Max(worstSeek, d)
				}
			}
			t.Logf("%s: %d frames, full-read error vs truth %.2f ms, worst frame-time error after seeks %.2f ms (sampleSize=%d blockAlign=%d)",
				c.name, len(full), maxErr*1000, worstSeek*1000, a.SampleSize, a.BlockAlign)
			if maxErr > 0.005 || worstSeek > 0.005 {
				t.Errorf("timing error too large")
			}
			// ffmpeg (parser re-packetizes into frames): every ffprobe packet time should
			// coincide with one of our frame times
			pk := ffprobePackets(t, path)
			perr, j := 0.0, 0
			for _, p := range pk {
				for j < len(full)-1 && full[j+1].t <= p.pts+1e-6 {
					j++
				}
				perr = math.Max(perr, math.Min(math.Abs(p.pts-full[j].t), math.Abs(p.pts-full[min(j+1, len(full)-1)].t)))
			}
			t.Logf("  vs ffprobe (%d packets): max distance to our frame times %.2f ms", len(pk), perr*1000)
		})
	}
}

func itoa(i int) string { return string(bytes.TrimSpace([]byte(func() string { return fmtInt(i) }()))) }
func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}
