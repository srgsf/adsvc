package demux

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"time"

	"github.com/srgsf/adsvc/internal/mediatime"
)

const (
	tsPacket     = 188
	m2tsPacket   = 192      // a 4-byte timestamp, then a TS packet
	tsHeadWindow = 4 << 20  // bytes from the start in which every stream's first PTS is looked for
	tsMaxPES     = 4 << 20  // one audio PES
	tsMaxSection = 16 << 10 // PAT/PMT
	ptsWrap      = 1 << 33

	tsResyncChunk = 4 << 10        // bytes added to the search window at a time while out of sync
	tsResyncKeep  = 3 * m2tsPacket // of the window, what may still hold the start of a sync
)

// tsContainer is a push-based MPEG transport stream demuxer (TS and 192-byte M2TS).
//
// The PAT and PMT repeat throughout the stream, so they are picked up from any range.
// Audio PES packets carry their own PTS, so after a seek the demuxer just resyncs on the
// packet stride and waits for the next PES start. Times are relative to the stream's first
// PTS, which is read from the start of the file (every player reads it first), the way
// players and ffmpeg put the start of a TS at zero.
type tsContainer struct {
	sink Sink

	stride int // 188 or 192 (M2TS: 4-byte timestamp before each packet); 0 = unknown
	pos    int64
	buf    []byte // bytes carried between writes: a partial packet, or the resync window
	synced bool

	pmtPID    int
	programs  bool
	audioPID  int
	esPIDs    map[int]bool
	sections  map[int][]byte
	firstPTS  map[int]int64 // first PTS per elementary stream, from the head of the file
	startPTS  int64         // -1 until known
	fromStart bool          // this run began at offset 0
	headEnd   int64
	held      []heldPES // audio PES seen before the start PTS was known

	pes    []byte // audio PES being collected (header included)
	pesOK  bool   // it started with a PUSI we saw
	lastCC int
	delay  frameDelay
	err    error
}

type heldPES struct {
	pts  int64
	data []byte
}

func newTSContainer(sink Sink) *tsContainer {
	c := &tsContainer{sink: sink, pmtPID: -1, audioPID: -1, startPTS: -1, lastCC: -1,
		esPIDs: map[int]bool{}, sections: map[int][]byte{}, firstPTS: map[int]int64{}}
	c.delay.emit = sink.Frame
	return c
}

func (c *tsContainer) Range(off int64) {
	c.endPES(false)
	if c.fromStart && c.startPTS < 0 {
		c.decideStart()
	}
	if err := c.delay.flush(); err != nil {
		c.fail(err)
	}
	c.pos, c.buf, c.synced, c.lastCC = off, c.buf[:0], false, -1
	c.fromStart = off == 0
	c.headEnd = off + tsHeadWindow
	for k := range c.sections {
		delete(c.sections, k)
	}
}

func (c *tsContainer) Close() error {
	c.endPES(true)
	if c.startPTS < 0 {
		c.decideStart()
	}
	if err := c.delay.flush(); err != nil {
		c.fail(err)
	}
	c.sink.Close()
	return c.err
}

func (c *tsContainer) Duration() time.Duration { return 0 }

func (c *tsContainer) fail(err error) {
	if c.err == nil {
		c.err = fmt.Errorf("ts: %w", err)
	}
}

func (c *tsContainer) Write(p []byte) error {
	// Packets are parsed in place in p; buf holds only what spans writes: a partial
	// packet, or while out of sync the window being searched.
	for c.err == nil {
		if !c.synced {
			if len(p) == 0 {
				break
			}
			n := min(len(p), tsResyncChunk)
			c.buf, p = append(c.buf, p[:n]...), p[n:]
			if !c.sync() {
				if drop := len(c.buf) - tsResyncKeep; drop > 0 {
					c.buf = c.buf[:copy(c.buf, c.buf[drop:])]
					c.pos += int64(drop)
				}
				continue
			}
		}
		if len(c.buf) > 0 { // the packets carried over first
			if len(c.buf) < c.stride {
				n := min(c.stride-len(c.buf), len(p))
				c.buf, p = append(c.buf, p[:n]...), p[n:]
				if len(c.buf) < c.stride {
					break
				}
			}
			if c.step(c.buf[:c.stride]) {
				c.buf = c.buf[:copy(c.buf, c.buf[c.stride:])]
			}
			continue // out of sync: buf is where the search starts
		}
		for len(p) >= c.stride && c.step(p[:c.stride]) {
			p = p[c.stride:]
		}
		if c.synced {
			c.buf = append(c.buf, p...)
			break
		}
	}
	if c.fromStart && c.startPTS < 0 && c.pos >= c.headEnd {
		c.decideStart()
	}
	return c.err
}

// step parses one packet at the stride (pkt holds c.stride bytes); it reports false, and
// drops sync, when there is no sync byte where the packet should begin.
func (c *tsContainer) step(pkt []byte) bool {
	pkt = pkt[c.stride-tsPacket:]
	if pkt[0] != 0x47 {
		c.synced = false
		c.lastCC = -1
		c.pes, c.pesOK = c.pes[:0], false
		return false
	}
	c.packet(pkt)
	c.pos += int64(c.stride)
	return true
}

// sync finds three sync bytes one packet apart and positions buf at the packet start.
func (c *tsContainer) sync() bool {
	strides := []int{tsPacket, m2tsPacket}
	if c.stride != 0 {
		strides = strides[:0]
		strides = append(strides, c.stride)
	}
	for i := 0; i+2*tsPacket < len(c.buf); i++ {
		for _, st := range strides {
			s := i + st - tsPacket // the 0x47 of the first packet (M2TS: after 4 bytes)
			if s+2*st >= len(c.buf) {
				continue
			}
			if c.buf[s] == 0x47 && c.buf[s+st] == 0x47 && c.buf[s+2*st] == 0x47 {
				c.stride, c.synced = st, true
				c.buf = c.buf[i:]
				c.pos += int64(i)
				return true
			}
		}
	}
	return false
}

func (c *tsContainer) packet(pkt []byte) {
	pusi := pkt[1]&0x40 != 0
	pid := int(pkt[1]&0x1F)<<8 | int(pkt[2])
	afc := pkt[3] >> 4 & 3
	cc := int(pkt[3] & 0x0F)
	if pkt[1]&0x80 != 0 || pkt[3]&0xC0 != 0 {
		return // transport error or scrambled
	}
	payload := pkt[4:]
	switch afc {
	case 0, 2:
		return // no payload
	case 3:
		if len(payload) < 1 || int(payload[0]) >= len(payload) {
			return
		}
		payload = payload[1+int(payload[0]):]
	}
	switch {
	case pid == 0 || pid == c.pmtPID:
		c.section(pid, pusi, payload)
	case pid == c.audioPID:
		c.audio(pusi, cc, payload)
	case pusi && c.fromStart && c.startPTS < 0 && c.esPIDs[pid]:
		if _, seen := c.firstPTS[pid]; !seen {
			if pts, ok := pesPTS(payload); ok {
				c.firstPTS[pid] = pts
				c.maybeStart()
			}
		}
	}
}

// section assembles PAT/PMT sections, which may span packets.
func (c *tsContainer) section(pid int, pusi bool, payload []byte) {
	if pusi {
		if len(payload) < 1 || int(payload[0]) >= len(payload) {
			return
		}
		payload = payload[1+int(payload[0]):]
		c.sections[pid] = append(c.sections[pid][:0], payload...)
	} else if b, ok := c.sections[pid]; ok && len(b) > 0 {
		c.sections[pid] = append(b, payload...)
	} else {
		return
	}
	b := c.sections[pid]
	if len(b) < 3 {
		return
	}
	total := 3 + (int(b[1]&0x0F)<<8 | int(b[2]))
	if total > tsMaxSection {
		delete(c.sections, pid)
		return
	}
	if len(b) < total {
		return
	}
	sec := b[:total]
	delete(c.sections, pid)
	if total < 12 || crc32MPEG(sec) != 0 {
		return
	}
	switch sec[0] {
	case 0x00:
		c.pat(sec)
	case 0x02:
		c.pmt(sec)
	}
}

func (c *tsContainer) pat(sec []byte) {
	for i := 8; i+4 <= len(sec)-4; i += 4 {
		program := binary.BigEndian.Uint16(sec[i:])
		if program != 0 {
			c.pmtPID = int(binary.BigEndian.Uint16(sec[i+2:]) & 0x1FFF)
			return
		}
	}
}

func (c *tsContainer) pmt(sec []byte) {
	if c.programs {
		return
	}
	pil := int(binary.BigEndian.Uint16(sec[10:]) & 0x0FFF)
	i := 12 + pil
	type es struct {
		pid    int
		format AudioFormat
		name   string
	}
	var audio []es
	for i+5 <= len(sec)-4 {
		st := sec[i]
		pid := int(binary.BigEndian.Uint16(sec[i+1:]) & 0x1FFF)
		n := int(binary.BigEndian.Uint16(sec[i+3:]) & 0x0FFF)
		if i+5+n > len(sec)-4 {
			return
		}
		desc := sec[i+5 : i+5+n]
		i += 5 + n
		c.esPIDs[pid] = true
		if f, name := tsAudioFormat(st, desc); name != "" {
			audio = append(audio, es{pid, f, name})
		}
	}
	c.programs = true
	if len(audio) == 0 {
		slog.Warn("ts: no supported audio stream")
		return
	}
	a := audio[0]
	if err := c.sink.Configure(a.format); err != nil {
		c.fail(err)
		return
	}
	c.audioPID = a.pid
	slog.Info("ts: audio stream", "pid", a.pid, "codec", a.name, "packet_size", c.stride)
}

// tsAudioFormat maps a PMT stream type (and its descriptors) to a raw ffmpeg input.
func tsAudioFormat(st byte, desc []byte) (AudioFormat, string) {
	codec := tsStreamTypes[st]
	if st == 0x06 { // DVB / system B: the descriptors say what it is
		for len(desc) >= 2 && codec == "" {
			tag, n := desc[0], int(desc[1])
			if 2+n > len(desc) {
				break
			}
			if codec = tsDescriptors[tag]; codec == "" && tag == 0x05 && n >= 4 { // registration
				codec = tsRegistrations[string(desc[2:6])]
			}
			desc = desc[2+n:]
		}
	}
	return rawFormat(codec), codec
}

// PMT stream types, DVB descriptor tags and registration descriptors of the audio codecs
// (rawFormat) a TS can carry.
var (
	tsStreamTypes = map[byte]string{
		0x03: codecMP3, 0x04: codecMP3, 0x0F: codecADTS, 0x11: codecLATM, 0x81: codecAC3,
		0x84: codecEAC3, 0x87: codecEAC3, 0xA1: codecEAC3, 0x82: codecDTS, 0x85: codecDTS, 0x86: codecDTS, 0xA2: codecDTS,
	}
	tsDescriptors   = map[byte]string{0x6A: codecAC3, 0x7A: codecEAC3, 0x7B: codecDTS, 0x7C: codecLATM}
	tsRegistrations = map[string]string{"AC-3": codecAC3, "EAC3": codecEAC3, "DTS1": codecDTS, "DTS2": codecDTS, "DTS3": codecDTS}
)

// audio collects the audio PES packets.
func (c *tsContainer) audio(pusi bool, cc int, payload []byte) {
	if cc == c.lastCC {
		return // a duplicate packet (allowed once by the standard)
	}
	if c.lastCC >= 0 && cc != (c.lastCC+1)&0x0F {
		c.pes, c.pesOK = c.pes[:0], false // lost packets: this PES is broken
	}
	c.lastCC = cc
	if pusi {
		c.endPES(true)
		c.pes, c.pesOK = append(c.pes[:0], payload...), true
		return
	}
	if c.pesOK {
		if len(c.pes)+len(payload) > tsMaxPES {
			c.pes, c.pesOK = c.pes[:0], false
			return
		}
		c.pes = append(c.pes, payload...)
	}
}

// endPES hands a collected PES on. complete says the next PES has started (or the stream
// ended); otherwise the PES is used only if its declared length shows it is whole.
func (c *tsContainer) endPES(complete bool) {
	b := c.pes
	c.pes, c.pesOK = c.pes[:0], false
	if len(b) < 9 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return
	}
	hlen := 9 + int(b[8])
	if hlen > len(b) {
		return
	}
	if n := int(binary.BigEndian.Uint16(b[4:])); n > 0 {
		if 6+n > len(b) {
			return // truncated
		}
		b = b[:6+n]
	} else if !complete {
		return
	}
	pts, ok := pesPTS(b)
	data := b[hlen:]
	if !ok || len(data) == 0 {
		return
	}
	if c.startPTS < 0 {
		if c.fromStart {
			if _, seen := c.firstPTS[c.audioPID]; !seen {
				c.firstPTS[c.audioPID] = pts
			}
			c.held = append(c.held, heldPES{pts, append([]byte(nil), data...)})
			c.maybeStart()
		}
		return
	}
	c.push(pts, data)
}

func (c *tsContainer) push(pts int64, data []byte) {
	rel := (pts - c.startPTS) & (ptsWrap - 1)
	if rel >= ptsWrap/2 {
		rel -= ptsWrap // before the start
	}
	if err := c.delay.push(mediatime.Ticks(rel, 1, 90000), 0, nil, data); err != nil {
		c.fail(err)
	}
}

// maybeStart fixes the start PTS once every stream has shown its first PTS.
func (c *tsContainer) maybeStart() {
	if !c.programs || c.startPTS >= 0 {
		return
	}
	for pid := range c.esPIDs {
		if _, ok := c.firstPTS[pid]; !ok {
			return
		}
	}
	c.decideStart()
}

// decideStart takes the earliest first PTS seen as the start (the player's zero) and
// releases the audio held until then.
func (c *tsContainer) decideStart() {
	if len(c.firstPTS) == 0 {
		return
	}
	var ref int64 = -1
	for _, p := range c.firstPTS {
		if ref < 0 {
			ref = p
			continue
		}
		// earlier modulo the 33-bit wrap
		if d := (p - ref) & (ptsWrap - 1); d >= ptsWrap/2 {
			ref = p
		}
	}
	c.startPTS = ref
	held := c.held
	c.held = nil
	for _, h := range held {
		c.push(h.pts, h.data)
	}
}

// pesPTS reads the PTS of a PES packet that starts at b.
func pesPTS(b []byte) (int64, bool) {
	if len(b) < 14 || b[0] != 0 || b[1] != 0 || b[2] != 1 {
		return 0, false
	}
	switch b[3] {
	case 0xBC, 0xBE, 0xBF, 0xF0, 0xF1, 0xF2, 0xF8, 0xFF:
		return 0, false // no optional header
	}
	if b[7]&0x80 == 0 {
		return 0, false
	}
	p := b[9:14]
	pts := int64(p[0]>>1&7)<<30 | int64(p[1])<<22 | int64(p[2]>>1)<<15 | int64(p[3])<<7 | int64(p[4]>>1)
	return pts, true
}

var crcTable = func() (t [256]uint32) {
	for i := range t {
		c := uint32(i) << 24
		for range 8 {
			if c&0x80000000 != 0 {
				c = c<<1 ^ 0x04C11DB7
			} else {
				c <<= 1
			}
		}
		t[i] = c
	}
	return
}()

// crc32MPEG is the CRC of PSI sections; over a section including its CRC it is 0.
func crc32MPEG(b []byte) uint32 {
	c := uint32(0xFFFFFFFF)
	for _, x := range b {
		c = c<<8 ^ crcTable[byte(c>>24)^x]
	}
	return c
}
