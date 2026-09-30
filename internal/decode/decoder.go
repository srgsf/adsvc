// Package decode runs ffmpeg over demuxed audio frames and scans the PCM for ads.
//
// Self-framed codecs go to ffmpeg raw; everything else is rewrapped into an audio-only
// Matroska stream, so a minimal ffmpeg build needs few demuxers.
package decode

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/srgsf/adsvc/internal/demux"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/fingerprint"
	"github.com/srgsf/adsvc/internal/library"
)

// Decoder feeds demuxed audio frames to ffmpeg and scans the PCM it produces.
// A new decoder is started for each continuous run of frames (after seeks/discontinuities).
//
// A run ends in one of two ways: Close closes ffmpeg's input, so everything written so far
// is still decoded and scanned; or the decoder's context ends (shutdown), which kills
// ffmpeg and drops what was not scanned yet.
type Decoder struct {
	ctx context.Context
	o   Options
	cur *rawRun
}

// Options configure a Decoder; they are fixed once it is created.
type Options struct {
	FFmpeg   ffmpeg.Tools
	Lib      *library.Library
	MinScore int
	Format   demux.AudioFormat // see SetFormat
	// OnBlock gets the result of every analysed block, from one goroutine at a time (a run's
	// scanner; the next run starts after the previous one ended).
	OnBlock func(detect.BlockResult)
	// Tap, when set, receives a copy of the decoded PCM of every run (see PCMRing).
	Tap *PCMRing
}

type rawRun struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	w    *bufio.Writer // frames are small (0.3-4 KB): batch them into fewer pipe writes
	done chan struct{}
	t0   time.Duration
	tmp  []byte // Matroska: scratch for building a block
}

// New creates a decoder; its ffmpeg runs end with ctx.
func New(ctx context.Context, o Options) *Decoder { return &Decoder{ctx: ctx, o: o} }

// SetFormat sets how frames reach ffmpeg; it applies to the next run.
func (d *Decoder) SetFormat(f demux.AudioFormat) { d.o.Format = f }

// Start begins a new continuous run whose first sample is at media time t0.
func (d *Decoder) Start(t0 time.Duration) error {
	d.Close()
	// the input format is known: skip probing so decoding (and detection) starts immediately
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error",
		"-probesize", "32768", "-analyzeduration", "0"}, d.o.Format.InputArgs()...)
	args = append(args, "-i", "pipe:0", "-af", "aresample=async=1", "-ac", "1",
		"-ar", strconv.Itoa(fingerprint.SampleRate), "-f", "s16le", "pipe:1")
	if err := d.ctx.Err(); err != nil {
		return err // shutting down: no new runs
	}
	cmd := exec.CommandContext(d.ctx, d.o.FFmpeg.Bin(), args...)
	stderr := &ffmpeg.TailBuffer{Max: 4 << 10}
	cmd.Stderr = stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		ffmpegRuns.With("start_failed").Inc()
		return err
	}
	started := time.Now()
	ffmpegRunning.Inc()
	var src io.Reader = out
	if d.o.Tap != nil {
		d.o.Tap.Reset(t0)
		src = io.TeeReader(out, d.o.Tap)
	}
	r := &rawRun{cmd: cmd, in: in, w: bufio.NewWriterSize(in, 64<<10), done: make(chan struct{}), t0: t0}
	if m := d.o.Format.Matroska; m != nil {
		_, _ = r.w.Write(m.AppendHeader(nil)) // a write error surfaces on the first frame
	}
	slog.Debug("decoder: run started", "t0", t0, "input", d.o.Format.InputArgs())
	go func() {
		defer close(r.done)
		defer ffmpegRunning.Dec()
		reached, scanErr := detect.Scan(d.ctx, src, t0, d.o.Lib, d.o.MinScore, 0, d.o.OnBlock)
		err := cmd.Wait()
		ffmpegRunSeconds.Since(started)
		switch {
		case d.ctx.Err() != nil:
			ffmpegRuns.With("shutdown").Inc()
			slog.Debug("decoder: run stopped by shutdown", "t0", t0, "reached", reached)
		case err != nil:
			ffmpegRuns.With("failed").Inc()
			slog.Warn("decoder: ffmpeg failed", "t0", t0, "reached", reached, "err", err,
				"stderr", strings.TrimSpace(stderr.String()))
		case scanErr != nil:
			ffmpegRuns.With("read_error").Inc()
			slog.Warn("decoder: reading decoded audio", "t0", t0, "reached", reached, "err", scanErr)
		default:
			ffmpegRuns.With("ok").Inc()
			slog.Debug("decoder: run ended", "t0", t0, "reached", reached)
		}
	}()
	d.cur = r
	return nil
}

// Write passes one encoded frame of a raw stream to the current run.
func (d *Decoder) Write(p []byte) error {
	return d.WriteFrame(0, p)
}

// WriteFrame passes one encoded frame, presented at media time t, to the current run.
// The time only matters for rewrapped (Matroska) streams.
func (d *Decoder) WriteFrame(t time.Duration, p []byte) error {
	r := d.cur
	if r == nil {
		return errors.New("decoder not started")
	}
	if d.o.Format.Matroska == nil {
		_, err := r.w.Write(p)
		return err
	}
	r.tmp = demux.AppendMatroskaFrame(r.tmp[:0], (t - r.t0).Round(time.Millisecond).Milliseconds(), p)
	_, err := r.w.Write(r.tmp)
	return err
}

// Close ends the current run and waits until its audio has been scanned.
func (d *Decoder) Close() {
	if d.cur == nil {
		return
	}
	d.cur.w.Flush()
	d.cur.in.Close()
	<-d.cur.done
	d.cur = nil
}
