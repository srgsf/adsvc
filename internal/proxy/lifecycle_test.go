package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/store"
	"github.com/srgsf/adsvc/internal/testmedia"
)

// The end of the proxy's context shuts it down on its own; Close then only waits.
func TestShutdownOnContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	payload := bytes.Repeat([]byte("not a media file, just bytes."), 5000)
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x.bin", time.Now(), bytes.NewReader(payload))
	}))
	defer src.Close()
	ctx, cancel := context.WithCancel(t.Context())
	px, err := New(ctx, Config{FFmpeg: testmedia.Decoder(), DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	const ih = "0123456789abcdef0123456789abcdef01234567"
	u := front.URL + "/s?ih=" + ih + "&idx=1&u=" + urlEscape(src.URL+"/x.bin")
	fetch := func() {
		t.Helper()
		resp, err := http.Get(u)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !bytes.Equal(b, payload) {
			t.Fatalf("status %d, %d of %d bytes", resp.StatusCode, len(b), len(payload))
		}
	}
	fetch()

	cancel()
	done := make(chan error, 1)
	go func() { done <- px.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the proxy did not shut down when its context ended")
	}

	// A request after shutdown is still forwarded, without a session or analysis.
	fetch()
	px.mu.Lock()
	n := len(px.sess)
	px.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d sessions after shutdown", n)
	}

	// The session's map was stored before the catalogue closed.
	st, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if m, err := st.Maps.Lookup(t.Context(), "ih:"+ih+"/1"); err != nil || m == nil || m.Size != int64(len(payload)) {
		t.Fatalf("stored map %+v, %v", m, err)
	}
}
