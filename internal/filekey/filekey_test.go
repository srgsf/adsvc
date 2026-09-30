package filekey

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
)

const testHash = "0123456789abcdef0123456789abcdef01234567"

func mustURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestTorrServerKey(t *testing.T) {
	upper := strings.ToUpper(testHash)
	b32 := "AERUKZ4JVPG66AJDIVTYTK6N54ASGRLH" // testHash in base32
	for _, c := range []struct{ in, want string }{
		{"http://ts:8090/play/" + upper + "/2", "ih:" + testHash + "/2"},
		{"http://ts/prefix/play/" + testHash + "/3", "ih:" + testHash + "/3"},
		{"http://ts/stream/Film.mkv?link=" + upper + "&index=2&play", "ih:" + testHash + "/2"},
		{"http://ts/stream?link=" + url.QueryEscape("magnet:?xt=urn:btih:"+b32+"&dn=x") + "&index=1&play", "ih:" + testHash + "/1"},
		{"http://ts/stream?link=" + testHash + "&stat", ""},                // no index: not a file
		{"http://ts/stream?link=torrs://abc&index=1&play", ""},             // torrs hash: opaque
		{"http://cdn.example/play/notahash/1", ""},                         // not a torrent
		{"http://cdn.example/video.mkv?link=" + testHash + "&index=1", ""}, // not /stream
	} {
		if got := FromTorrServerURL(mustURL(t, c.in)); got != c.want {
			t.Errorf("FromTorrServerURL(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormUpstream(t *testing.T) {
	a := NormalizeURL(mustURL(t, "https://user:pw@CDN.example/v/1.mkv?token=abc&b=2&a=1&Expires=9#t=5"))
	b := NormalizeURL(mustURL(t, "https://cdn.example/v/1.mkv?a=1&expires=10&b=2&token=zzz"))
	if a != b || a != "https://cdn.example/v/1.mkv?a=1&b=2" {
		t.Fatalf("NormalizeURL: %q vs %q", a, b)
	}
	r := RedactURL(mustURL(t, "https://user:pw@cdn.example/v?token=secret&a=1"))
	if strings.Contains(r, "secret") || strings.Contains(r, "pw") {
		t.Fatalf("RedactURL leaks: %s", r)
	}
}

func TestBuilder(t *testing.T) {
	data := make([]byte, 300<<10)
	for i := range data {
		data[i] = byte(i * 7)
	}
	var kb Builder
	kb.Feed(0, data[:1000]) // head, in pieces
	kb.Feed(1000, data[1000:EdgeSize])
	if _, ok := kb.Done(); ok {
		t.Fatal("no key without the size")
	}
	kb.SetSize(int64(len(data)))
	if _, ok := kb.Done(); ok {
		t.Fatal("no key before the tail has been seen")
	}
	kb.Feed(int64(len(data))-1000, data[len(data)-1000:]) // a partial tail proves nothing
	if _, ok := kb.Done(); ok {
		t.Fatal("no key from a partial tail")
	}
	kb.Feed(int64(len(data))-EdgeSize, data[len(data)-EdgeSize:])
	got, ok := kb.Done()
	if !ok || got != contentKey(data) {
		t.Fatalf("key = %q %v, want %q", got, ok, contentKey(data))
	}
}

func contentKey(data []byte) string {
	h := sha256.New()
	var sz [8]byte
	binary.LittleEndian.PutUint64(sz[:], uint64(len(data)))
	h.Write(sz[:])
	h.Write(data[:EdgeSize])
	h.Write(data[len(data)-EdgeSize:])
	return "c:" + hex.EncodeToString(h.Sum(nil))
}
