package avi

import "testing"

// buildIx builds the body of an OpenDML ix## standard index (without its 8-byte header).
func buildIx(base int64, chunks [][2]int64) []byte {
	b := make([]byte, 24, 24+8*len(chunks))
	le.PutUint16(b, 2) // wLongsPerEntry
	b[3] = 1           // AVI_INDEX_OF_CHUNKS
	le.PutUint32(b[4:], uint32(len(chunks)))
	le.PutUint32(b[8:], fourcc("01wb"))
	le.PutUint64(b[12:], uint64(base))
	for _, c := range chunks {
		var e [8]byte
		le.PutUint32(e[:], uint32(c[0]-base+8)) // entries point at the chunk data
		le.PutUint32(e[4:], uint32(c[1]))
		b = append(b, e[:]...)
	}
	return b
}

// TestAddIxAt covers a seek in an OpenDML file: the player only downloads the ix## chunks
// of the segments it plays, so an index has to be usable without the ones before it.
func TestAddIxAt(t *testing.T) {
	a := &Audio{StreamID: 1, ChunkID: fourcc("01wb"), Codec: "mp3", SampleRate: 48000}
	a.setParams(3, 125, 0, 0) // VBR MP3: sampleSize 0
	a.BlockAlign = 1152

	seg := func(base int64, n int) ([][2]int64, []byte, int64) {
		var chunks [][2]int64
		var dur int64
		for i := range n {
			size := int64(576 + 8*i)
			chunks = append(chunks, [2]int64{base + int64(i)*4096, size})
			dur += a.ChunkDuration(size)
		}
		return chunks, buildIx(base, chunks), dur
	}
	c0, ix0, dur0 := seg(10_000, 40)
	c1, ix1, _ := seg(1_000_000, 40)
	a.ODMLIndex = []int64{9000, 999_000}
	a.ODMLDur = []int64{dur0, 0}

	// a player that reads the file from the start collects both indexes, in order
	full := NewIndex(a)
	full.AddIx(ix0)
	full.AddIx(ix1)
	if full.Len() != len(c0)+len(c1) {
		t.Fatalf("full index has %d chunks, want %d", full.Len(), len(c0)+len(c1))
	}

	// a player that seeks into the second segment only sees the second index
	part := NewIndex(a)
	part.AddIxAt(ix1, a.ODMLStart(1))
	if part.Len() != len(c1) {
		t.Fatalf("partial index has %d chunks, want %d", part.Len(), len(c1))
	}
	for i := 0; i < part.Len(); i++ {
		j := full.Len() - part.Len() + i
		if part.Chunks[i].Off != full.Chunks[j].Off || part.Chunks[i].Time != full.Chunks[j].Time {
			t.Fatalf("chunk %d: offset %d time %d, want offset %d time %d",
				i, part.Chunks[i].Off, part.Chunks[i].Time, full.Chunks[j].Off, full.Chunks[j].Time)
		}
	}
	t.Logf("second segment starts at %.3fs in both", a.Time(part.Chunks[0].Time).Seconds())

	// out of order: the offsets must stay sorted so seeking (binary search) still works
	mixed := NewIndex(a)
	mixed.AddIxAt(ix1, a.ODMLStart(1))
	mixed.AddIxAt(ix0, a.ODMLStart(0))
	if mixed.Len() != full.Len() {
		t.Fatalf("mixed index has %d chunks, want %d", mixed.Len(), full.Len())
	}
	for i := 0; i < mixed.Len(); i++ {
		if mixed.Chunks[i].Off != full.Chunks[i].Off || mixed.Chunks[i].Time != full.Chunks[i].Time {
			t.Fatalf("chunk %d: %d/%d, want %d/%d", i, mixed.Chunks[i].Off, mixed.Chunks[i].Time, full.Chunks[i].Off, full.Chunks[i].Time)
		}
	}
	if k := mixed.ChunkAtOrAfter(1_000_000); k != len(c0) {
		t.Fatalf("ChunkAtOrAfter = %d, want %d", k, len(c0))
	}
}
