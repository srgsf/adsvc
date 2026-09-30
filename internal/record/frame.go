package record

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Wire framing of records (sync streams and pushes): the JSON body travels as the exact
// bytes that were signed, inside a binary frame:
//
//	uvarint(n) then n bytes: uvarint(len kind) kind, origin (32), uvarint(seq),
//	varint(ts), uvarint(len body) body, signature (64)

// MaxFrame bounds one frame: the biggest record is an ad of about 20 KB of landmarks,
// base64 in JSON.
const MaxFrame = 256 << 10

var errFrame = errors.New("record: corrupt frame")

// AppendFrame appends the frame of r to dst.
func AppendFrame(dst []byte, r *Record) []byte {
	var p []byte
	p = binary.AppendUvarint(p, uint64(len(r.Kind)))
	p = append(p, r.Kind...)
	p = append(p, r.Origin[:]...)
	p = binary.AppendUvarint(p, r.Seq)
	p = binary.AppendVarint(p, r.TS)
	p = binary.AppendUvarint(p, uint64(len(r.Body)))
	p = append(p, r.Body...)
	p = append(p, r.Sig[:]...)
	dst = binary.AppendUvarint(dst, uint64(len(p)))
	return append(dst, p...)
}

// ReadFrame reads one frame. It returns io.EOF at a clean end of the stream, and an error
// for anything else, including a truncated frame. The signature is not checked.
func ReadFrame(br *bufio.Reader) (Record, error) {
	n, err := readMinimalUvarint(br)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return Record{}, io.EOF
		}
		return Record{}, fmt.Errorf("%w: %w", errFrame, err)
	}
	if n == 0 || n > MaxFrame {
		return Record{}, fmt.Errorf("%w: %d bytes", errFrame, n)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(br, p); err != nil {
		return Record{}, fmt.Errorf("%w: %w", errFrame, err)
	}
	return parseFrame(p, true)
}

// ParseFrame decodes the payload of one frame (after its length). The record does not
// keep p.
func ParseFrame(p []byte) (Record, error) { return parseFrame(p, false) }

// parseFrame is ParseFrame; with own, p is the caller's to give away and the body is a
// slice of it rather than a copy.
func parseFrame(p []byte, own bool) (Record, error) {
	var r Record
	take := func(n uint64) ([]byte, bool) {
		if n > uint64(len(p)) {
			return nil, false
		}
		b := p[:n]
		p = p[n:]
		return b, true
	}
	// Only minimal varints (what AppendFrame writes): one record has one framing.
	uv := func() (uint64, bool) {
		v, k := binary.Uvarint(p)
		if k <= 0 || (k > 1 && p[k-1] == 0) {
			return 0, false
		}
		p = p[k:]
		return v, true
	}
	kl, ok := uv()
	if !ok || kl == 0 || kl > 32 {
		return r, errFrame
	}
	kind, _ := take(kl)
	r.Kind = string(kind)
	o, ok := take(uint64(len(r.Origin)))
	if !ok {
		return r, errFrame
	}
	copy(r.Origin[:], o)
	if r.Seq, ok = uv(); !ok {
		return r, errFrame
	}
	ts, k := binary.Varint(p)
	if k <= 0 || (k > 1 && p[k-1] == 0) {
		return r, errFrame
	}
	r.TS, p = ts, p[k:]
	bl, ok := uv()
	if !ok {
		return r, errFrame
	}
	body, ok := take(bl)
	if !ok {
		return r, errFrame
	}
	r.Body = body
	if !own {
		r.Body = append([]byte(nil), body...)
	}
	sig, ok := take(uint64(len(r.Sig)))
	if !ok || len(p) != 0 {
		return r, errFrame
	}
	copy(r.Sig[:], sig)
	return r, nil
}

// readMinimalUvarint is binary.ReadUvarint restricted to the minimal encoding.
func readMinimalUvarint(br io.ByteReader) (uint64, error) {
	var v uint64
	for i := range binary.MaxVarintLen64 {
		c, err := br.ReadByte()
		if err != nil {
			if i > 0 && errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			if i > 0 && c == 0 {
				return 0, errors.New("padded varint")
			}
			return v, nil
		}
	}
	return 0, errors.New("varint overflows 64 bits")
}
