package decode

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/srgsf/adsvc/internal/demux"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/library"
)

// seamTol is how far a frame time may drift from the expected continuation before the
// decoder is restarted. Small gaps (a resync inside a chunk, a range the player re-read)
// are not worth a restart; a real seek moves the timeline by much more.
const seamTol = 500 * time.Millisecond

// Sink decodes the frames a Container produces and scans them for ads.
// One ffmpeg run covers one continuous stretch of media time.
type Sink struct {
	dec    *Decoder
	ring   *PCMRing
	skip   func(t time.Duration) bool // already analysed: don't spend a decoder on it
	ready  bool
	run    bool
	expect time.Duration
	skipAt time.Duration
}

// NewSink creates a sink that decodes with ff, scans with lib and taps the PCM into ring.
// skip reports media times that have been analysed already. Its ffmpeg runs end with ctx.
func NewSink(ctx context.Context, ff ffmpeg.Tools, lib *library.Library, minScore int, ring *PCMRing,
	onBlock func(detect.BlockResult), skip func(t time.Duration) bool) *Sink {
	s := &Sink{ring: ring, skip: skip}
	s.dec = New(ctx, Options{FFmpeg: ff, Lib: lib, MinScore: minScore, OnBlock: onBlock, Tap: ring})
	return s
}

// Configure implements demux.Sink.
func (s *Sink) Configure(f demux.AudioFormat) error {
	if !f.Valid() {
		return errors.New("adsvc: empty audio format")
	}
	s.dec.SetFormat(f)
	s.ready = true
	return nil
}

// Frame implements demux.Sink: it restarts the decoder whenever the timeline jumps.
func (s *Sink) Frame(f demux.Frame) error {
	if !s.ready {
		return errors.New("adsvc: frames before the audio format is known")
	}
	if s.skip != nil && s.skip(f.Time) {
		idleFrames.Inc()
		if s.run {
			s.dec.Close()
			s.run, s.skipAt = false, f.Time
			slog.Debug("decoder idle: already analysed", "t", f.Time)
		}
		return nil
	}
	if !s.run || (f.Time-s.expect).Abs() > seamTol {
		if s.run {
			decoderRestarts.Inc()
		}
		if err := s.dec.Start(f.Time); err != nil {
			return err
		}
		s.run = true
	}
	if err := s.dec.WriteFrame(f.Time, f.Data); err != nil {
		return err
	}
	s.expect = f.Time + f.Dur
	return nil
}

// Close ends the current decoder run.
func (s *Sink) Close() {
	if s.run {
		s.dec.Close()
		s.run = false
	}
}
