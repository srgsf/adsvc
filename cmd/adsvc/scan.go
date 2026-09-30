package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/admap"
	"github.com/srgsf/adsvc/internal/decode"
	"github.com/srgsf/adsvc/internal/demux"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/ffmpeg"
	"github.com/srgsf/adsvc/internal/filekey"
	"github.com/srgsf/adsvc/internal/library"
	"github.com/srgsf/adsvc/internal/store"
)

// runScan prints the ads found in a file or URL and stores its ad map, as a proxy session
// would: under the file's content key, all of it analysed and every ad confirmed, so that
// playing it through the proxy later skips them without decoding anything. Containers
// adsvc knows (AVI, Matroska, MP4, TS) are demuxed by its own parsers, as in the proxy,
// so the minimal ffmpeg is enough; anything else is left to ffmpeg, which then needs a
// build that can demux it.
func runScan(args []string) error {
	fl := flag.NewFlagSet("scan", flag.ExitOnError)
	dir := dataDirFlag(fl, defaultDataDir)
	in := fl.String("in", "", "input file or URL")
	minScore := fl.Int("minscore", detect.DefaultMinScore, "match threshold")
	verbose := fl.Bool("v", false, "print every block")
	dryRun := fl.Bool("n", false, "only print the ads: do not store the file's ad map")
	ffmpegBin := fl.String("ffmpeg", bundledFFmpeg(), "ffmpeg binary (the minimal build is enough for AVI, Matroska, MP4 and TS)")
	setupLog := logFlags(fl, slog.LevelInfo)
	if err := parse(fl, args, setupLog, false); err != nil {
		return err
	}
	if *in == "" {
		return usagef("need -in")
	}
	return withStore(*dir, func(ctx context.Context, st *store.Store) error {
		sc := &scanner{verbose: *verbose, confirmScore: detect.DefaultConfirmScore(*minScore)}
		ff := ffmpeg.Tools{FFmpegPath: *ffmpegBin}
		started := time.Now()
		err := sc.demuxed(ctx, ff, st.Lib, *minScore, *in)
		if errors.Is(err, demux.ErrUnsupported) {
			slog.Info("container not recognised, ffmpeg demuxes it", "err", err)
			err = sc.whole(ctx, ff, st.Lib, *minScore, *in)
		}
		if err != nil {
			return err
		}
		el := time.Since(started)
		fmt.Printf("scanned %.1fs of audio in %s (%.0fx realtime)\n", sc.reached.Seconds(), el.Round(time.Millisecond), sc.reached.Seconds()/el.Seconds())
		if *dryRun {
			return nil
		}
		if sc.key == "" && !ffmpeg.IsHTTP(*in) {
			sc.key, sc.size, err = fileContentKey(*in)
			if err != nil {
				return err
			}
		}
		return sc.save(ctx, st.Maps)
	})
}

// scanner prints detections as blocks are scanned.
type scanner struct {
	verbose      bool
	confirmScore int

	mu       sync.Mutex // blocks are reported by the decoder's goroutines
	ads      []detect.Candidate
	covered  detect.Intervals
	reached  time.Duration
	duration time.Duration // as far as the container knows it

	key   string // content key of the file, "" until known
	alias []string
	size  int64
}

func (sc *scanner) onBlock(b detect.BlockResult) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.reached = max(sc.reached, b.To)
	sc.covered = sc.covered.Add(b.From, b.To)
	if sc.verbose {
		fmt.Printf("block [%7.1f,%7.1f) %d matches\n", b.From.Seconds(), b.To.Seconds(), len(b.Detections))
	}
	for _, d := range b.Detections {
		var changed bool
		if sc.ads, _, changed = detect.MergeCandidate(sc.ads, d, b.From, b.To); changed {
			fmt.Printf("AD %s %-24q %8.2f - %8.2f  score %3d  (decoded up to %.1fs)\n",
				d.AdID.Short(), d.Label, d.Start.Seconds(), d.End.Seconds(), d.Score, (b.To + detect.Overlap).Seconds())
		}
	}
}

// demuxed reads in through adsvc's demuxer for its container and decodes
// the audio frames with ff. It returns demux.ErrUnsupported (having read only the head)
// when the container is not one it knows.
func (sc *scanner) demuxed(ctx context.Context, ff ffmpeg.Tools, lib *library.Library, minScore int, in string) error {
	src, size, err := openInput(ctx, in)
	if err != nil {
		return err
	}
	defer src.Close()
	var kb filekey.Builder
	kb.SetSize(size)
	var off int64
	br := bufio.NewReaderSize(src, 256<<10)
	head, err := br.Peek(64 << 10)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	sink := decode.NewSink(ctx, ff, lib, minScore, nil, sc.onBlock, nil)
	cont, err := demux.New(demux.Sniff(head), sink)
	if err != nil {
		return err
	}
	slog.Info("container recognised", "container", demux.Sniff(head))
	cont.Range(0)
	buf := make([]byte, 256<<10)
	for {
		n, rerr := br.Read(buf)
		if n > 0 {
			kb.Feed(off, buf[:n])
			off += int64(n)
			if werr := cont.Write(buf[:n]); werr != nil {
				cont.Close() //nolint:errcheck // the same error as werr
				return fmt.Errorf("demux: %w", werr)
			}
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return errors.Join(rerr, cont.Close())
		}
	}
	if err := cont.Close(); err != nil { // flushes the last frames and waits until they are scanned
		return fmt.Errorf("demux: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sc.duration = cont.Duration()
	if size <= 0 || off == size { // a short read must not yield the key of another file
		kb.SetSize(off)
		sc.key, _ = kb.Done()
		sc.size = off
	}
	if u, err := url.Parse(in); err == nil && ffmpeg.IsHTTP(in) {
		if k := filekey.FromTorrServerURL(u); k != "" {
			sc.alias = append(sc.alias, k)
		}
	}
	return nil
}

// whole has ffmpeg read and decode in itself.
func (sc *scanner) whole(ctx context.Context, ff ffmpeg.Tools, lib *library.Library, minScore int, in string) error {
	s, err := ff.DecodePCM(ctx, in, 0, 0)
	if err != nil {
		return err
	}
	reached, err := detect.Scan(ctx, s, 0, lib, minScore, 0, sc.onBlock)
	sc.reached = max(sc.reached, reached)
	return errors.Join(err, s.Wait())
}

// save stores the ad map of the scanned file, merged with what was stored for it before
// (a proxy session may have seen other parts, or found ads that are gone from the library).
func (sc *scanner) save(ctx context.Context, maps *admap.Store) error {
	if sc.key == "" {
		slog.Warn("no content key for the input (its size is unknown or it was not read to the end): ad map not stored")
		return nil
	}
	sc.mu.Lock()
	defer sc.mu.Unlock()
	ads, covered, aliases := sc.ads, sc.covered, sc.alias
	old, err := maps.Lookup(ctx, append([]string{sc.key}, sc.alias...)...)
	if err != nil {
		return err
	}
	if old != nil {
		for _, r := range old.Analyzed {
			covered = covered.Add(r[0], r[1])
		}
		for _, d := range old.Ads {
			var i int
			ads, i, _ = detect.MergeCandidate(ads, d, detect.NoBlock, detect.NoBlock)
			ads[i].Sticky = ads[i].Sticky || d.Confirmed
		}
		for _, a := range old.Aliases {
			if a != sc.key && !slices.Contains(aliases, a) {
				aliases = append(aliases, a)
			}
		}
	}
	m := &admap.FileMap{Key: sc.key, Aliases: aliases, Size: sc.size,
		Duration: max(sc.duration, sc.reached), Analyzed: covered}
	confirmed := 0
	for i := range ads {
		d := ads[i].Detection
		d.Confirmed = ads[i].Confirm(covered, sc.confirmScore)
		if d.Confirmed {
			confirmed++
		}
		m.Ads = append(m.Ads, d)
	}
	if err := maps.Put(ctx, m); err != nil {
		return err
	}
	fmt.Printf("stored the ad map of %s: %d ads, %d confirmed\n", sc.key, len(m.Ads), confirmed)
	return nil
}

// fileContentKey is the content key of a local file (filekey.Builder over its ends).
func fileContentKey(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	var kb filekey.Builder
	kb.SetSize(fi.Size())
	for _, off := range []int64{0, max(0, fi.Size()-filekey.EdgeSize)} {
		buf := make([]byte, min(fi.Size(), filekey.EdgeSize))
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", 0, err
		}
		kb.Feed(off, buf[:n])
	}
	key, ok := kb.Done()
	if !ok {
		return "", 0, fmt.Errorf("%s: could not read both ends", path)
	}
	return key, fi.Size(), nil
}

// openInput opens a local file or an http(s) URL for reading, with its size (0 when the
// server does not say).
func openInput(ctx context.Context, in string) (io.ReadCloser, int64, error) {
	if !ffmpeg.IsHTTP(in) {
		f, err := os.Open(in)
		if err != nil {
			return nil, 0, err
		}
		fi, err := f.Stat()
		if err != nil {
			return nil, 0, errors.Join(err, f.Close())
		}
		return f, fi.Size(), nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, in, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", ffmpeg.UserAgent)
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: the URL is the user's own -in
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, 0, fmt.Errorf("GET %s: %s", filekey.RedactURL(req.URL), resp.Status)
	}
	return resp.Body, max(resp.ContentLength, 0), nil
}
