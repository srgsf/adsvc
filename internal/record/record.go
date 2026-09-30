// Package record is the replication unit of the catalogue (docs/database.md §2): an
// immutable record, signed by the node that wrote it (its origin), with a JSON body.
// Records are stored verbatim, relayed between peers as they are, and materialized into
// the catalogue's tables by every node that accepts them.
package record

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strings"
)

// Kinds of records.
const (
	KindAdAdd     = "ad.add"     // a fingerprint (AdAdd)
	KindAdLabel   = "ad.label"   // a label for an ad (AdLabel), last writer wins
	KindAdVote    = "ad.vote"    // a vote on an ad (AdVote), the latest per origin counts
	KindAdDup     = "ad.dup"     // an ad is a near-duplicate of another (AdDup)
	KindAdRetract = "ad.retract" // the author withdraws its ad (AdRetract)
	KindFileMap   = "file.map"   // the ads an origin found in a file (FileMap)
)

// Origin identifies the node that wrote a record: its ed25519 public key.
type Origin [ed25519.PublicKeySize]byte

// String is the key in hex.
func (o Origin) String() string { return hex.EncodeToString(o[:]) }

// Short is the first 8 hex digits, for logs.
func (o Origin) Short() string { return hex.EncodeToString(o[:4]) }

// ParseOrigin parses the hex form of String.
func ParseOrigin(s string) (Origin, error) {
	var o Origin
	b, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != len(o) {
		return o, fmt.Errorf("origin %q: want %d hex digits", s, 2*len(o))
	}
	copy(o[:], b)
	return o, nil
}

// MarshalText encodes o in hex.
func (o Origin) MarshalText() ([]byte, error) { return []byte(o.String()), nil }

// UnmarshalText decodes hex.
func (o *Origin) UnmarshalText(b []byte) error {
	v, err := ParseOrigin(string(b))
	if err == nil {
		*o = v
	}
	return err
}

// Record is one signed record.
type Record struct {
	Kind   string
	Origin Origin
	Seq    uint64 // per origin, from 1, never reused
	TS     int64  // unix milliseconds from the origin's hybrid logical clock
	Body   []byte // JSON (see body.go), exactly as signed
	Sig    [ed25519.SignatureSize]byte
}

const domain = "adsvc record v1\x00"

// signed is what the signature covers: every field but the signature, unambiguously
// framed, so no two records share it.
func (r *Record) signed() []byte {
	b := make([]byte, 0, len(domain)+len(r.Kind)+1+len(r.Origin)+16+len(r.Body))
	b = append(b, domain...)
	b = append(b, r.Kind...)
	b = append(b, 0)
	b = append(b, r.Origin[:]...)
	b = binary.BigEndian.AppendUint64(b, r.Seq)
	b = binary.BigEndian.AppendUint64(b, uint64(r.TS))
	return append(b, r.Body...)
}

// Hash identifies the record: SHA-256 of what is signed.
func (r *Record) Hash() [32]byte { return sha256.Sum256(r.signed()) }

// Verify reports whether the signature is the origin's.
func (r *Record) Verify() bool {
	return ed25519.Verify(r.Origin[:], r.signed(), r.Sig[:])
}

// Sign fills in r.Origin and r.Sig with id.
func (id *Identity) Sign(r *Record) {
	r.Origin = id.Origin
	copy(r.Sig[:], ed25519.Sign(id.key, r.signed()))
}
