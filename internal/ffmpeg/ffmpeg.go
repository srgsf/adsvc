// Package ffmpeg runs the external ffmpeg binary (no CGO): decoding to the PCM the
// fingerprinter wants.
package ffmpeg

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/fingerprint"
)

// UserAgent is what adsvc's own requests of a source (ffmpeg's, scan's) say they are.
const UserAgent = "adsvc/0.1"

// Tools locates the external ffmpeg binary. The zero value uses the one on PATH.
type Tools struct {
	FFmpegPath string // default "ffmpeg"
}

// Bin is the ffmpeg binary to run.
func (f Tools) Bin() string {
	if f.FFmpegPath == "" {
		return "ffmpeg"
	}
	return f.FFmpegPath
}

// IsHTTP reports whether an input is an http(s) URL rather than a local path.
func IsHTTP(in string) bool {
	return strings.HasPrefix(in, "http://") || strings.HasPrefix(in, "https://")
}

// PCMStream is a running ffmpeg process producing mono s16le at fingerprint.SampleRate on
// stdout.
type PCMStream struct {
	io.Reader
	cmd    *exec.Cmd
	stderr *TailBuffer
}

// Wait waits for ffmpeg to exit and returns its error with stderr attached.
func (p *PCMStream) Wait() error {
	if err := p.cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %w: %s", err, strings.TrimSpace(p.stderr.String()))
	}
	return nil
}

// DecodePCM starts ffmpeg decoding the first audio track of input from `from` for `dur`
// (dur<=0: until the end). Sample n of the output is at from+n/SampleRate.
func (f Tools) DecodePCM(ctx context.Context, input string, from, dur time.Duration) (*PCMStream, error) {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if IsHTTP(input) {
		args = append(args, "-user_agent", UserAgent, "-rw_timeout", "30000000")
	}
	if from > 0 {
		args = append(args, "-ss", secs(from))
	}
	if dur > 0 {
		args = append(args, "-t", secs(dur))
	}
	args = append(args, "-i", input,
		"-map", "0:a:0", "-vn", "-sn", "-dn",
		"-af", "aresample=async=1",
		"-ac", "1", "-ar", strconv.Itoa(fingerprint.SampleRate), "-f", "s16le", "pipe:1")
	cmd := exec.CommandContext(ctx, f.Bin(), args...)
	stderr := &TailBuffer{Max: 4 << 10} // a broken input can make ffmpeg write megabytes
	cmd.Stderr = stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &PCMStream{Reader: out, cmd: cmd, stderr: stderr}, nil
}

// secs writes d for ffmpeg's -ss and -t: seconds, to the microsecond.
func secs(d time.Duration) string { return strconv.FormatFloat(d.Seconds(), 'f', 6, 64) }

// TailBuffer keeps the last Max bytes written to it: ffmpeg's stderr, for logs and errors.
type TailBuffer struct {
	mu  sync.Mutex
	Max int
	buf []byte
}

// Write implements io.Writer.
func (b *TailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.Max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

// String is what was kept.
func (b *TailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
