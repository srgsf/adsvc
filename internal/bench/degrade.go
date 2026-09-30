package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/fingerprint"
)

// Degrade transforms the programme audio before analysis (the ads stay as enrolled), to
// measure robustness to what other releases of the same programme do to it.
type Degrade struct {
	Name   string
	Filter string   // audio filter applied first
	Encode []string // output args of an intermediate lossy encode (nil: none)
	Speed  float64  // how much faster the result plays than the file (0: not changed)
}

// Degrades are the transformations bench -degrade knows. Encoding needs a full ffmpeg.
var Degrades = []Degrade{
	{Name: "none"},
	{Name: "aac48", Encode: []string{"-ac", "1", "-c:a", "aac", "-b:a", "48k", "-f", "adts"}},
	{Name: "mp3-32", Encode: []string{"-ac", "1", "-ar", "22050", "-c:a", "libmp3lame", "-b:a", "32k", "-f", "mp3"}},
	// 23.976 -> 25 fps PAL speed-up: pitch rises by the same 4.3%.
	{Name: "pal", Filter: "aresample=48000,asetrate=50050,aresample=48000", Speed: 50050.0 / 48000},
	// The same speed-up with pitch correction, as some releases do it.
	{Name: "pal-tempo", Filter: "atempo=1.042709", Speed: 1.042709},
}

// DegradeByName looks up one of Degrades.
func DegradeByName(name string) (Degrade, bool) {
	for _, d := range Degrades {
		if d.Name == name {
			return d, true
		}
	}
	return Degrade{}, false
}

// scale maps labels from the file's timeline onto the degraded audio's.
func (d Degrade) scale(labels []Occurrence) []Occurrence {
	if d.Speed == 0 || d.Speed == 1 {
		return labels
	}
	out := make([]Occurrence, len(labels))
	for i, l := range labels {
		l.Start = time.Duration(float64(l.Start) / d.Speed)
		l.End = time.Duration(float64(l.End) / d.Speed)
		out[i] = l
	}
	return out
}

// cmdPipeline is a chain of ffmpeg processes whose last stdout is read.
type cmdPipeline struct {
	io.Reader
	cmds   []*exec.Cmd
	stderr []*bytes.Buffer
}

// Wait waits for every process and returns their errors with stderr attached.
func (p *cmdPipeline) Wait() error {
	var errs []error
	for i, c := range p.cmds {
		if err := c.Wait(); err != nil {
			errs = append(errs, fmt.Errorf("ffmpeg stage %d: %w: %s", i+1, err, strings.TrimSpace(p.stderr[i].String())))
		}
	}
	return errors.Join(errs...)
}

// decodeDegraded decodes the first audio track of input to mono s16le at SampleRate, through
// d's filter and lossy encode.
func decodeDegraded(ctx context.Context, f ffmpeg.Tools, input string, d Degrade) (*cmdPipeline, error) {
	common := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	toPCM := []string{"-ac", "1", "-ar", strconv.Itoa(fingerprint.SampleRate), "-f", "s16le", "pipe:1"}
	af := "aresample=async=1"
	first := append(append([]string{}, common...), "-i", input, "-map", "0:a:0", "-vn", "-sn", "-dn")
	var stages [][]string
	if d.Encode == nil {
		if d.Filter != "" {
			af = d.Filter + "," + af
		}
		stages = [][]string{append(append(first, "-af", af), toPCM...)}
	} else {
		if d.Filter != "" {
			first = append(first, "-af", d.Filter)
		}
		first = append(append(first, d.Encode...), "pipe:1")
		second := append(append(append([]string{}, common...), "-i", "pipe:0", "-af", af), toPCM...)
		stages = [][]string{first, second}
	}
	p := &cmdPipeline{}
	var prev io.Reader
	for _, args := range stages {
		cmd := exec.CommandContext(ctx, f.Bin(), args...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		cmd.Stdin = prev
		out, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			for _, c := range p.cmds {
				_ = c.Process.Kill() // cleaning up after a failed start: best effort
				_ = c.Wait()
			}
			return nil, err
		}
		p.cmds, p.stderr = append(p.cmds, cmd), append(p.stderr, &stderr)
		prev = out
	}
	p.Reader = prev
	return p, nil
}
