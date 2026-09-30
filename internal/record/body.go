package record

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/srgsf/adsvc/internal/adtype"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/fingerprint"
)

// Record bodies. Times are integer milliseconds. Decoding ignores unknown fields, so a
// newer node can add optional fields without breaking older ones.
type (
	// AdAdd is a fingerprint; its id is the content hash of Points.
	AdAdd struct {
		ID         fingerprint.ID  `json:"id"`
		FPVersion  int             `json:"fp_version"`
		DurationMs int32           `json:"duration_ms"`
		Points     []byte          `json:"points"` // fingerprint.Encode, base64 in JSON
		Label      string          `json:"label,omitempty"`
		Type       string          `json:"type,omitempty"` // adtype; empty = ad
		Source     *Source         `json:"source,omitempty"`
		Supersedes *fingerprint.ID `json:"supersedes,omitempty"` // the same ad with older parameters

		landmarks int // len of the decoded Points, set by Decode
	}
	// Source is the file and range an ad was enrolled from.
	Source struct {
		Key     string `json:"key"`
		StartMs int32  `json:"start_ms"`
		EndMs   int32  `json:"end_ms"`
	}
	// AdLabel labels an ad, and may change its type.
	AdLabel struct {
		Ad    fingerprint.ID `json:"ad"`
		Label string         `json:"label"`
		Type  string         `json:"type,omitempty"` // adtype; empty = leave as it is
	}
	// AdVote is a vote on an ad, optionally about one detection of it (Key, StartMs).
	AdVote struct {
		Ad      fingerprint.ID `json:"ad"`
		Value   int            `json:"value"` // +1 or -1
		Reason  string         `json:"reason"`
		Key     string         `json:"key,omitempty"`
		StartMs *int32         `json:"start_ms,omitempty"`
	}
	// AdDup says Ad is a near-duplicate of Canonical.
	AdDup struct {
		Ad        fingerprint.ID `json:"ad"`
		Canonical fingerprint.ID `json:"canonical"`
	}
	// AdRetract withdraws an ad; only its author's counts.
	AdRetract struct {
		Ad fingerprint.ID `json:"ad"`
	}
	// FileMap is what an origin found in a file; it replaces that origin's earlier map of
	// the file. There is no URL or name: files are keys.
	FileMap struct {
		Key        string      `json:"key"`
		Aliases    []string    `json:"aliases,omitempty"`
		FPVersion  int         `json:"fp_version"`
		Size       int64       `json:"size,omitempty"`
		DurationMs int32       `json:"duration_ms,omitempty"`
		Analyzed   [][2]int32  `json:"analyzed,omitempty"`
		Ads        []Detection `json:"ads"`
	}
	// Detection is one ad in a file.
	Detection struct {
		Ad        fingerprint.ID `json:"ad"`
		StartMs   int32          `json:"start_ms"`
		EndMs     int32          `json:"end_ms"`
		Score     int            `json:"score"`
		Confirmed bool           `json:"confirmed"`
	}
)

// Vote reasons.
const (
	ReasonGood     = "good"
	ReasonNotAd    = "not_ad"
	ReasonBoundary = "boundary"
	ReasonDup      = "dup"
)

// Limits of a valid record.
const (
	MaxBody         = 200 << 10
	MaxLabel        = 200 // runes
	MaxKey          = 128 // bytes
	MaxAdDurationMs = 10 * 60 * 1000
	MinAdLandmarks  = 50
	maxAliases      = 16
	maxDetections   = 1000
	maxAnalyzed     = 10000
	minPositionMs   = -60_000           // an ad may start a little before the stream does
	minTS           = 1_577_836_800_000 // 2020-01-01: nothing older is an adsvc record
	maxFPVersion    = 1 << 16
	maxKindLen      = 32
)

// Why a record is rejected; peers count rejections by reason (who sends trash).
const (
	RejectSignature = "signature" // not signed by its origin
	RejectInvalid   = "invalid"   // malformed envelope or body
	RejectFuture    = "future"    // time too far ahead
	RejectBlocked   = "blocked"   // origin blocked by this node
	RejectQuota     = "quota"     // origin over its daily quota
)

// Rejection is why a record was not accepted.
type Rejection struct {
	Reason string
	Err    error
}

func (e *Rejection) Error() string { return e.Reason + ": " + e.Err.Error() }
func (e *Rejection) Unwrap() error { return e.Err }

func invalid(format string, args ...any) error {
	return &Rejection{RejectInvalid, fmt.Errorf(format, args...)}
}

// Check validates r as received: signature, envelope, and the body of a known kind.
// Records of unknown kinds (from newer nodes) pass with a valid envelope: they are
// stored and relayed, not materialized.
func Check(r *Record, now time.Time) error {
	_, err := CheckDecode(r, now)
	return err
}

// CheckDecode is Check, and returns the decoded body (see Decode).
func CheckDecode(r *Record, now time.Time) (Body, error) {
	if !r.Verify() {
		return nil, &Rejection{RejectSignature, errors.New("bad signature")}
	}
	switch {
	case !validKind(r.Kind):
		return nil, invalid("kind %q", r.Kind)
	case r.Seq == 0:
		return nil, invalid("seq 0")
	case r.TS < minTS:
		return nil, invalid("time %d before 2020", r.TS)
	case r.TS > now.Add(MaxSkew).UnixMilli():
		return nil, &Rejection{RejectFuture, fmt.Errorf("time %d is %s ahead", r.TS, time.UnixMilli(r.TS).Sub(now).Round(time.Second))}
	case len(r.Body) > MaxBody:
		return nil, invalid("body of %d bytes", len(r.Body))
	case len(r.Body) == 0 || r.Body[0] != '{' || !json.Valid(r.Body):
		return nil, invalid("body is not a JSON object")
	}
	return Decode(r)
}

func validKind(k string) bool {
	if k == "" || len(k) > maxKindLen {
		return false
	}
	for _, c := range k {
		if (c < 'a' || c > 'z') && c != '.' && c != '_' {
			return false
		}
	}
	return true
}

// Body is a decoded record body of a kind this version knows.
type Body interface {
	// AdIDs are the ads the record is about (their history lists it).
	AdIDs() []fingerprint.ID
	validate() error
}

// bodies makes an empty body of each known kind: a new kind is registered here.
var bodies = map[string]func() Body{
	KindAdAdd:     func() Body { return new(AdAdd) },
	KindAdLabel:   func() Body { return new(AdLabel) },
	KindAdVote:    func() Body { return new(AdVote) },
	KindAdDup:     func() Body { return new(AdDup) },
	KindAdRetract: func() Body { return new(AdRetract) },
	KindFileMap:   func() Body { return new(FileMap) },
}

// AdIDs is the ad added.
func (a *AdAdd) AdIDs() []fingerprint.ID { return []fingerprint.ID{a.ID} }

// AdIDs is the ad labelled.
func (l *AdLabel) AdIDs() []fingerprint.ID { return []fingerprint.ID{l.Ad} }

// AdIDs is the ad voted on.
func (v *AdVote) AdIDs() []fingerprint.ID { return []fingerprint.ID{v.Ad} }

// AdIDs is the duplicate and its canonical ad.
func (d *AdDup) AdIDs() []fingerprint.ID { return []fingerprint.ID{d.Ad, d.Canonical} }

// AdIDs is the ad withdrawn.
func (r *AdRetract) AdIDs() []fingerprint.ID { return []fingerprint.ID{r.Ad} }

// AdIDs is none: file maps are listed by file key, not under their ads.
func (m *FileMap) AdIDs() []fingerprint.ID { return nil }

// Decode decodes and validates the body of r: *AdAdd, *AdLabel, *AdVote, *AdDup,
// *AdRetract or *FileMap; nil (and no error) for a kind this version does not know.
func Decode(r *Record) (Body, error) {
	mk := bodies[r.Kind]
	if mk == nil {
		return nil, nil
	}
	v := mk()
	if err := json.Unmarshal(r.Body, v); err != nil {
		return nil, invalid("%s: %v", r.Kind, err)
	}
	if err := v.validate(); err != nil {
		return nil, invalid("%s: %v", r.Kind, err)
	}
	return v, nil
}

// New is an unsigned record of kind with body (see Identity.Sign). Bodies are encoded
// without HTML escaping: the bytes are signed as they are.
func New(kind string, seq uint64, ts int64, body any) (Record, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		return Record{}, err
	}
	return Record{Kind: kind, Seq: seq, TS: ts, Body: bytes.TrimSuffix(b.Bytes(), []byte("\n"))}, nil
}

func errIf(cond bool, format string, args ...any) error {
	if cond {
		return fmt.Errorf(format, args...)
	}
	return nil
}

func checkLabel(l string) error {
	if utf8.RuneCountInString(l) > MaxLabel {
		return fmt.Errorf("label longer than %d characters", MaxLabel)
	}
	if strings.IndexFunc(l, unicode.IsControl) >= 0 {
		return errors.New("label has control characters")
	}
	return nil
}

// checkType accepts an adtype or "" (not given).
func checkType(t string) error {
	return errIf(t != "" && !adtype.Valid(t), "type %q", t)
}

func checkKey(k string) error {
	return errIf(len(k) > MaxKey || !filekey.Stored(k), "file key %q is not ih:, c: or id: form", k)
}

func checkRange(start, end int32) error {
	return errIf(start < minPositionMs || end <= start, "range %d..%d ms", start, end)
}

// Landmarks is the number of landmarks in Points (known once Decode validated them).
func (a *AdAdd) Landmarks() int { return a.landmarks }

func (a *AdAdd) validate() error {
	switch {
	case a.ID.IsZero():
		return errors.New("no id")
	case a.FPVersion < 1 || a.FPVersion > maxFPVersion:
		return fmt.Errorf("fingerprint version %d", a.FPVersion)
	case a.DurationMs <= 0 || a.DurationMs > MaxAdDurationMs:
		return fmt.Errorf("duration %d ms", a.DurationMs)
	case a.Supersedes != nil && (a.Supersedes.IsZero() || *a.Supersedes == a.ID):
		return errors.New("supersedes")
	}
	pts, err := fingerprint.Decode(a.Points)
	if err != nil {
		return err
	}
	a.landmarks = len(pts)
	if len(pts) < MinAdLandmarks {
		return fmt.Errorf("%d landmarks", len(pts))
	}
	if fingerprint.IDFor(a.FPVersion, a.Points) != a.ID {
		return errors.New("id is not the hash of the landmarks")
	}
	if err := checkLabel(a.Label); err != nil {
		return err
	}
	if err := checkType(a.Type); err != nil {
		return err
	}
	if a.Source != nil {
		if err := checkKey(a.Source.Key); err != nil {
			return err
		}
		return checkRange(a.Source.StartMs, a.Source.EndMs)
	}
	return nil
}

func (l *AdLabel) validate() error {
	if l.Ad.IsZero() {
		return errors.New("no ad")
	}
	if err := checkType(l.Type); err != nil {
		return err
	}
	return checkLabel(l.Label)
}

func (v *AdVote) validate() error {
	switch {
	case v.Ad.IsZero():
		return errors.New("no ad")
	case v.Value != 1 && v.Value != -1:
		return fmt.Errorf("value %d", v.Value)
	case v.Reason != ReasonGood && v.Reason != ReasonNotAd && v.Reason != ReasonBoundary && v.Reason != ReasonDup:
		return fmt.Errorf("reason %q", v.Reason)
	case v.Key != "":
		return checkKey(v.Key)
	}
	return nil
}

func (d *AdDup) validate() error {
	return errIf(d.Ad.IsZero() || d.Canonical.IsZero() || d.Ad == d.Canonical, "ad and canonical")
}

func (r *AdRetract) validate() error { return errIf(r.Ad.IsZero(), "no ad") }

func (m *FileMap) validate() error {
	if err := checkKey(m.Key); err != nil {
		return err
	}
	switch {
	case len(m.Aliases) > maxAliases || len(m.Ads) > maxDetections || len(m.Analyzed) > maxAnalyzed:
		return errors.New("too many aliases, ads or ranges")
	case m.FPVersion < 1 || m.FPVersion > maxFPVersion:
		return fmt.Errorf("fingerprint version %d", m.FPVersion)
	case m.Size < 0 || m.DurationMs < 0:
		return errors.New("negative size or duration")
	}
	for _, a := range m.Aliases {
		if err := checkKey(a); err != nil {
			return err
		}
	}
	for _, r := range m.Analyzed {
		if err := checkRange(r[0], r[1]); err != nil {
			return err
		}
	}
	for _, d := range m.Ads {
		if d.Ad.IsZero() {
			return errors.New("detection without an ad")
		}
		if err := checkRange(d.StartMs, d.EndMs); err != nil {
			return err
		}
	}
	return nil
}
