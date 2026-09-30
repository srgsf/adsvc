// Package filekey names files: a content key built from the bytes a player downloads
// anyway, torrent keys (infohash + file index, also recognised in TorrServer URLs), and the
// normalised or redacted forms of upstream URLs.
package filekey

import (
	"crypto/sha256"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"strings"
	"sync"
)

// EdgeSize is how much of each end of a file goes into a content key.
const EdgeSize = 64 << 10

// Builder computes the content key SHA-256(size | first 64 KiB | last 64 KiB) from the
// bytes the player downloads anyway. The head is free (every player reads the file header);
// the tail arrives with the index of most containers (AVI idx1, Matroska Cues, MP4 moov at
// the end). Until both ends have been seen the session uses a provisional key.
type Builder struct {
	mu   sync.Mutex
	size int64
	head []byte
	tail []byte
	key  string
}

// SetSize records the file size; the first known size wins.
func (k *Builder) SetSize(size int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if size > 0 && k.size == 0 {
		k.size = size
	}
}

// Feed offers the bytes at absolute offset off. Only bytes that extend an end
// contiguously are kept, so a partial read can never poison the key.
func (k *Builder) Feed(off int64, p []byte) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != "" {
		return
	}
	k.head = appendEdge(k.head, 0, off, p)
	if k.size > EdgeSize {
		k.tail = appendEdge(k.tail, k.size-EdgeSize, off, p)
	}
}

// appendEdge extends buf, which holds the bytes of [base, base+len(buf)), with p.
func appendEdge(buf []byte, base, off int64, p []byte) []byte {
	have := base + int64(len(buf))
	if off > have || off+int64(len(p)) <= have || len(buf) >= EdgeSize {
		return buf
	}
	p = p[have-off:]
	if n := EdgeSize - len(buf); len(p) > n {
		p = p[:n]
	}
	return append(buf, p...)
}

// Done reports the content key once both ends and the size are known.
func (k *Builder) Done() (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.key != "" {
		return k.key, true
	}
	wantHead := min(k.size, int64(EdgeSize))
	if k.size <= 0 || int64(len(k.head)) < wantHead {
		return "", false
	}
	if k.size > EdgeSize && len(k.tail) < EdgeSize {
		return "", false
	}
	h := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(k.size))
	h.Write(sz[:])
	h.Write(k.head)
	h.Write(k.tail)
	k.key = "c:" + hex.EncodeToString(h.Sum(nil))
	k.head, k.tail = nil, nil
	return k.key, true
}

// Opaque is the file key for a file id chosen by the client (the id parameter). The id is
// hashed: it could be a name, and keys are stored in a catalogue that may be shared.
func Opaque(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "id:" + hex.EncodeToString(sum[:16])
}

// Stored reports whether key is a form that may be stored: a torrent, content or opaque
// key (never a URL, a name or a provisional session key).
func Stored(key string) bool {
	return strings.HasPrefix(key, "ih:") || strings.HasPrefix(key, "c:") || strings.HasPrefix(key, "id:")
}

// Torrent is the file key for a torrent file index; it needs no reading at all.
func Torrent(infohash, index string) string {
	if infohash == "" {
		return ""
	}
	if index == "" {
		index = "1"
	}
	return "ih:" + normInfohash(infohash) + "/" + index
}

// normInfohash turns a v1 infohash into lower-case hex (TorrServer's form); base32 hashes
// from magnet links are converted. Anything else is returned unchanged.
func normInfohash(h string) string {
	switch len(h) {
	case 40:
		if _, err := hex.DecodeString(h); err == nil {
			return strings.ToLower(h)
		}
	case 32:
		if b, err := base32.StdEncoding.DecodeString(strings.ToUpper(h)); err == nil {
			return hex.EncodeToString(b)
		}
	}
	return h
}

// FromTorrServerURL recognises TorrServer stream URLs, so a client that only wraps the URL it
// would play anyway still gets a torrent key (and the stored ad map) from the first byte:
//
//	/stream[/name]?link=<infohash|magnet>&index=N
//	/play/<infohash>/<N>
func FromTorrServerURL(u *url.URL) string {
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if n := len(parts); n >= 3 && parts[n-3] == "play" {
		if ih := normInfohash(parts[n-2]); isHexHash(ih) {
			return Torrent(ih, parts[n-1])
		}
	}
	q := u.Query()
	if len(parts) == 0 || parts[0] != "stream" || q.Get("index") == "" {
		return ""
	}
	link := q.Get("link")
	if strings.HasPrefix(link, "magnet:") {
		if m, err := url.Parse(link); err == nil {
			for _, xt := range m.Query()["xt"] {
				if h, ok := strings.CutPrefix(xt, "urn:btih:"); ok {
					link = h
				}
			}
		}
	}
	if ih := normInfohash(link); isHexHash(ih) {
		return Torrent(ih, q.Get("index"))
	}
	return ""
}

func isHexHash(h string) bool {
	if len(h) != 40 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// VolatileParams are query parameters that carry credentials or expiry rather than
// identify content (signed and tokenised URLs). They are left out of a file's identity,
// so a re-signed URL is the same file, and their values never reach the logs.
var VolatileParams = []string{
	"token", "access_token", "auth", "key", "sig", "signature", "expires", "exp",
	"policy", "key-pair-id", "x-amz-signature", "x-amz-credential", "x-amz-date",
	"x-amz-expires", "x-amz-security-token", "x-amz-algorithm", "x-amz-signedheaders",
}

func volatile(name string) bool {
	for _, v := range VolatileParams {
		if strings.EqualFold(name, v) {
			return true
		}
	}
	return false
}

// NormalizeURL is the identity of an upstream URL: no user info, no volatile parameters,
// the rest sorted.
func NormalizeURL(u *url.URL) string {
	c := *u
	c.User = nil
	c.Fragment = ""
	q := c.Query()
	for k := range q {
		if volatile(k) {
			delete(q, k)
		}
	}
	c.RawQuery = q.Encode() // Encode sorts by key
	c.Host = strings.ToLower(c.Host)
	return c.String()
}

// RedactURL is u as it may appear in logs: no password, volatile values hidden.
func RedactURL(u *url.URL) string {
	c := *u
	q := c.Query()
	for k := range q {
		if volatile(k) {
			q[k] = []string{"REDACTED"}
		}
	}
	c.RawQuery = q.Encode()
	return c.Redacted()
}
