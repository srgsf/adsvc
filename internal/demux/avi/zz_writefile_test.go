package avi

import (
	"os"
	"strings"
	"testing"
)

// ADSVC_WRITE_CBR=in.ac3:out.avi go test -run TestWriteCBRFile ./avi
func TestWriteCBRFile(t *testing.T) {
	spec := os.Getenv("ADSVC_WRITE_CBR")
	if spec == "" {
		t.Skip()
	}
	p := strings.SplitN(spec, ":", 2)
	raw, err := os.ReadFile(p[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p[1], writeCBRAVI(raw, 0x2000, 2, 48000, 192000/8, 1000, 5), 0o644); err != nil {
		t.Fatal(err)
	}
}
