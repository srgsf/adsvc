package proxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/srgsf/adsvc/internal/testmedia"
)

func get(t *testing.T, u, token string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestReload(t *testing.T) {
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), Users: users("old")})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	lib := front.URL + "/ads/library"
	if code := get(t, lib, "old"); code != http.StatusOK {
		t.Fatalf("old key before reload: %d", code)
	}

	cfg := px.Config()
	cfg.Users = users("new")
	cfg.MaxStall = 5 * time.Second
	cfg.DataDir = "elsewhere"
	cfg.Client = nil
	restart := px.Reload(cfg)
	if !slices.Equal(restart, []string{"DataDir"}) {
		t.Errorf("restart required for %v, want [DataDir]", restart)
	}
	got := px.Config()
	if got.DataDir != "" || got.MaxStall != 5*time.Second || got.Client == nil || got.MinScore != 20 {
		t.Errorf("after reload: DataDir %q, MaxStall %v, Client %v, MinScore %d; want the old path, 5s, the old client, the default",
			got.DataDir, got.MaxStall, got.Client, got.MinScore)
	}
	if code := get(t, lib, "old"); code != http.StatusUnauthorized {
		t.Errorf("old key after reload: %d, want 401", code)
	}
	if code := get(t, lib, "new"); code != http.StatusOK {
		t.Errorf("new key after reload: %d, want 200", code)
	}

	cfg = px.Config()
	cfg.Users = nil
	cfg.AllowUpstream = func(u *url.URL) bool { return false }
	px.Reload(cfg)
	if code := get(t, lib, ""); code != http.StatusOK {
		t.Errorf("auth removed by reload, still %d", code)
	}
	if code := get(t, front.URL+"/s?u="+url.QueryEscape("http://any.example/x"), ""); code != http.StatusForbidden {
		t.Errorf("allow-list from reload not applied: %d", code)
	}
}

// Requests keep being served while the config is replaced (run with -race).
func TestReloadDuringRequests(t *testing.T) {
	px, err := New(t.Context(), Config{FFmpeg: testmedia.Decoder(), Users: users("a")})
	if err != nil {
		t.Fatal(err)
	}
	defer px.Close()
	front := httptest.NewServer(px.Handler())
	defer front.Close()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				// Both keys stay valid in every config below, so every request succeeds.
				if code := get(t, front.URL+"/ads/maps", "a"); code != http.StatusOK {
					t.Errorf("request during reload: %d", code)
					return
				}
			}
		})
	}
	for i := range 200 {
		cfg := px.Config()
		cfg.Users = users([]string{"a", "b"}[:1+i%2]...)
		cfg.ConfirmScore = 40 + i
		px.Reload(cfg)
	}
	wg.Wait()
}
