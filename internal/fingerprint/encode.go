package fingerprint

import (
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"uuid"
)

// Encode packs landmarks into their canonical form: sorted by (T, H) and grouped by anchor
// frame as uvarint(frame - previous frame), uvarint(count), then count hashes of 3 bytes
// (little-endian). About 3.2 bytes a landmark. Frames must not be negative and hashes must
// fit HashBits.
func Encode(pts []Point) ([]byte, error) {
	s := slices.Clone(pts)
	slices.SortFunc(s, func(a, b Point) int { return cmp.Or(cmp.Compare(a.T, b.T), cmp.Compare(a.H, b.H)) })
	out := make([]byte, 0, len(s)*7/2+16)
	prev := int32(0)
	for i := 0; i < len(s); {
		t := s[i].T
		if t < 0 {
			return nil, fmt.Errorf("landmark at frame %d", t)
		}
		j := i
		for j < len(s) && s[j].T == t {
			j++
		}
		out = binary.AppendUvarint(out, uint64(t-prev))
		out = binary.AppendUvarint(out, uint64(j-i))
		for _, p := range s[i:j] {
			if p.H>>HashBits != 0 {
				return nil, fmt.Errorf("hash %#x wider than %d bits", p.H, HashBits)
			}
			out = append(out, byte(p.H), byte(p.H>>8), byte(p.H>>16))
		}
		prev, i = t, j
	}
	return out, nil
}

var errCorrupt = errors.New("fingerprint: corrupt landmarks")

// Decode unpacks landmarks packed by Encode. Only the canonical form is accepted (frames
// increasing, hashes ascending within a frame, no trailing bytes), so that an ID computed
// from the decoded landmarks is the ID of b itself.
func Decode(b []byte) ([]Point, error) { return AppendDecode(nil, b) }

// AppendDecode is Decode appending to dst, so that a caller decoding many fingerprints
// can reuse one buffer. On error dst is returned unchanged.
func AppendDecode(dst []Point, b []byte) ([]Point, error) {
	pts := slices.Grow(dst, len(b)/3) // every landmark takes at least 3 bytes
	t := int64(0)
	for first := true; len(b) > 0; first = false {
		dt, n := uvarint(b)
		if n <= 0 || (dt == 0 && !first) || dt > 1<<31 {
			return dst, errCorrupt
		}
		b = b[n:]
		count, n := uvarint(b)
		if n <= 0 || count == 0 || count > uint64(len(b)-n)/3 {
			return dst, errCorrupt
		}
		b = b[n:]
		if t += int64(dt); t > 1<<31-1 {
			return dst, errCorrupt
		}
		prevH := uint32(0)
		for range count {
			h := uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16
			if h < prevH {
				return dst, errCorrupt
			}
			pts = append(pts, Point{H: h, T: int32(t)})
			prevH, b = h, b[3:]
		}
	}
	return pts, nil
}

// uvarint is binary.Uvarint restricted to the minimal encoding (which Encode writes):
// binary.Uvarint also accepts padded forms such as 0x81 0x00 for 1.
func uvarint(b []byte) (uint64, int) {
	v, n := binary.Uvarint(b)
	if n > 1 && b[n-1] == 0 {
		return 0, 0
	}
	return v, n
}

// ID identifies a fingerprint by its content: the first 16 bytes of
// SHA-256(Version ‖ Encode(points)). The same landmarks give the same ID wherever they
// were enrolled, and a relay cannot swap the landmarks under an ID.
type ID [16]byte

// IDOf is the ID of encoded landmarks (the output of Encode) of this Version.
func IDOf(encoded []byte) ID { return IDFor(Version, encoded) }

// IDFor is the ID of encoded landmarks computed with fingerprint parameters version (ads
// of other versions arrive from peers and keep their ids).
func IDFor(version int, encoded []byte) ID {
	h := sha256.New()
	var v [4]byte
	binary.LittleEndian.PutUint32(v[:], uint32(version))
	h.Write(v[:])
	h.Write(encoded)
	var id ID
	copy(id[:], h.Sum(nil))
	return id
}

// String is the UUID text form: 8-4-4-4-12 lowercase hex digits. The bytes are a hash,
// not an RFC 9562 UUID of any version; only the text form is shared (package uuid).
func (id ID) String() string { return uuid.UUID(id).String() }

// Short is the first 8 hex digits, for logs and labels.
func (id ID) Short() string { return hex.EncodeToString(id[:4]) }

// IsZero reports whether id is the zero ID (no ad).
func (id ID) IsZero() bool { return id == ID{} }

// ParseID parses the UUID text form, with or without dashes.
func ParseID(s string) (ID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("ad id %q: %w", s, err)
	}
	return ID(u), nil
}

// MarshalText encodes id in the UUID text form (JSON strings, logs).
func (id ID) MarshalText() ([]byte, error) { return uuid.UUID(id).MarshalText() }

// UnmarshalText decodes the UUID text form.
func (id *ID) UnmarshalText(b []byte) error {
	v, err := ParseID(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}
