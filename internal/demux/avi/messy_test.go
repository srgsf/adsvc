package avi

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// splitMP3Frames returns the frames of a raw MP3 stream.
func splitMP3Frames(es []byte) [][]byte {
	var out [][]byte
	for p := 0; p+4 <= len(es); {
		size, _, _, _, _, ok := mpegHeader(es[p:])
		if !ok || p+size > len(es) {
			p++
			continue
		}
		out = append(out, es[p:p+size])
		p += size
	}
	return out
}

// writeMessyVBR writes a "VBR" MP3 AVI (dwScale=1152, dwRate=sr, dwSampleSize=0,
// nBlockAlign=1152) but puts 2 frames in each chunk and a few garbage bytes between some
// frames, as badly behaved muxers do. Index time (1 block per chunk) then runs at half speed.
func writeMessyVBR(frames [][]byte, sr int) []byte {
	var chunks [][]byte
	for i := 0; i+1 < len(frames); i += 2 {
		var c []byte
		c = append(c, frames[i]...)
		if i%10 == 0 {
			c = append(c, 0, 0, 0) // junk bytes forcing a resync
		}
		c = append(c, frames[i+1]...)
		chunks = append(chunks, c)
	}
	return buildAVI(chunks, 0x55, 1, sr, 1152, uint32(sr), 0, 1152)
}

func buildAVI(chunks [][]byte, tag uint16, ch, sr int, scale, rate, sampleSize uint32, blockAlign uint16) []byte {
	w := func(b *bytes.Buffer, v any) { _ = binary.Write(b, binary.LittleEndian, v) } // a bytes.Buffer never fails
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
	var strh, strf, avih, strl, hdrl, movi, idx, body, f bytes.Buffer
	w(&avih, [14]uint32{0, 0, 0, 0x10})
	strh.WriteString("auds")
	w(&strh, [4]uint32{})
	w(&strh, [8]uint32{scale, rate, 0, uint32(len(chunks)), 0, 0, sampleSize, 0})
	w(&strh, [2]uint32{})
	w(&strf, []uint16{tag, uint16(ch)})
	w(&strf, []uint32{uint32(sr), 16000})
	w(&strf, []uint16{blockAlign, 0, 0})
	chunkf(&strl, "strh", strh.Bytes())
	chunkf(&strl, "strf", strf.Bytes())
	chunkf(&hdrl, "avih", avih.Bytes())
	hdrl.Write(list("strl", strl.Bytes()))
	for _, c := range chunks {
		idx.WriteString("00wb")
		w(&idx, []uint32{0x10, uint32(movi.Len() + 4), uint32(len(c))})
		chunkf(&movi, "00wb", c)
	}
	body.WriteString("AVI ")
	body.Write(list("hdrl", hdrl.Bytes()))
	body.Write(list("movi", movi.Bytes()))
	chunkf(&body, "idx1", idx.Bytes())
	f.WriteString("RIFF")
	w(&f, uint32(body.Len()))
	f.Write(body.Bytes())
	return f.Bytes()
}

func TestMessyVBR(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip()
	}
	dir := t.TempDir()
	es := filepath.Join(dir, "a.mp3")
	if b, err := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=r=44100",
		"-t", "120", "-c:a", "libmp3lame", "-q:a", "4", "-write_xing", "0", "-id3v2_version", "0", "-f", "mp3", es).CombinedOutput(); err != nil {
		t.Skipf("%v %s", err, b)
	}
	raw, _ := os.ReadFile(es)
	data := writeMessyVBR(splitMP3Frames(raw), 44100)
	if out := os.Getenv("ADSVC_WRITE_MESSY"); out != "" {
		if err := os.WriteFile(out, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	h, a, ix := loadIndex(t, data, false)
	full, disc := extract(h, a, ix, data, 0)
	last := full[len(full)-1].t
	x := NewExtractor(h, a, ix)
	x.OnFrame = func(Frame) {}
	x.Reset(0)
	x.Write(data)
	t.Logf("frames=%d last=%.2fs (true 120s; index says %.2fs) discontinuities=%d maxDrift=%.1fs", len(full), last, a.Time(ix.cum).Seconds(), disc, x.MaxDrift.Seconds())
	if last < 119 || last > 120.1 {
		t.Errorf("timeline should follow the frames (like the Java extractor), got %.2fs", last)
	}
	if disc > 1 {
		t.Errorf("continuous read produced %d discontinuities (each restarts the decoder)", disc)
	}
}
