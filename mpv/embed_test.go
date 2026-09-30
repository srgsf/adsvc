package mpv

import (
	"strings"
	"testing"
)

func TestScriptCarriesToken(t *testing.T) {
	if strings.Count(script, defaultToken) != 1 {
		t.Fatalf("adskip.lua must name %q once in its defaults", defaultToken)
	}
	got := string(Script(`to"k\en`))
	if !strings.Contains(got, `ads_token = "to\"k\\en"`) || strings.Contains(got, defaultToken) {
		t.Fatalf("token not written into the defaults:\n%s", got[:min(len(got), 1200)])
	}
}
