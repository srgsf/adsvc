package main

import (
	"cmp"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/srgsf/adsvc/internal/bench"
	"github.com/srgsf/adsvc/internal/detect"
	"github.com/srgsf/adsvc/internal/store"
)

// runBench scores the detector against labelled files (see bench/README.md).
func runBench(args []string) error {
	fl := flag.NewFlagSet("bench", flag.ExitOnError)
	labels := fl.String("labels", "", "ground truth, JSON (see bench/README.md)")
	dir := dataDirFlag(fl, "")
	fl.Lookup("data-dir").Usage = "data directory with the ads (default: the labels file's data_dir, else " + defaultDataDir + ")"
	mode := fl.String("mode", "offline", "offline: ffmpeg decodes each file | proxy: stream it through the pass-through proxy")
	degrade := fl.String("degrade", "none", "offline only: "+degradeNames())
	minScore := fl.Int("minscore", detect.DefaultMinScore, "match threshold")
	confirm := fl.Int("confirm", 0, "score that confirms from two blocks (default 2x minscore)")
	floor := fl.Int("floor", 5, "lowest candidate score recorded for the noise measure")
	jobs := fl.Int("j", max(1, runtime.NumCPU()/2), "files analysed in parallel")
	ffmpegBin := fl.String("ffmpeg", "", "decoder (default: ffmpeg on PATH offline, the bundled one for -mode proxy)")
	only := fl.String("only", "", "only files whose path contains this")
	jsonOut := fl.String("json", "", "also write runs and summary to this file")
	draft := fl.String("draft", "", "write draft labels for the files given as arguments to this file, then exit")
	verbose := fl.Bool("v", false, "print every labelled occurrence")
	setupLog := logFlags(fl, slog.LevelWarn) // a proxy run would log every session at info
	if err := parse(fl, args, setupLog, true); err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	if *draft != "" {
		return benchDraft(ctx, *draft, cmp.Or(*dir, defaultDataDir), fl.Args(), *minScore, *jobs)
	}
	if fl.NArg() > 0 {
		return usagef("unexpected arguments (files are only taken with -draft): %s", strings.Join(fl.Args(), " "))
	}
	if *labels == "" {
		return usagef("need -labels (or -draft out.json files...)")
	}
	set, err := bench.Load(*labels)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cmp.Or(*dir, set.DataDir, defaultDataDir))
	if err != nil {
		return err
	}
	defer st.Close()
	lib := st.Lib
	for _, w := range set.Check(lib) {
		slog.Warn("labels do not match the library", "problem", w)
	}
	*confirm = cmp.Or(*confirm, detect.DefaultConfirmScore(*minScore))
	opts := bench.Options{MinScore: *minScore, ConfirmScore: *confirm, Floor: *floor}
	switch *mode {
	case "offline":
		d, ok := bench.DegradeByName(*degrade)
		if !ok {
			return usagef("unknown -degrade %q (%s)", *degrade, degradeNames())
		}
		opts.Degrade = d
		opts.FFmpeg.FFmpegPath = *ffmpegBin
	case "proxy":
		if *degrade != "none" {
			return usagef("-degrade needs -mode offline")
		}
		if *ffmpegBin == "" {
			*ffmpegBin = bundledFFmpeg()
		}
		opts.FFmpeg.FFmpegPath = *ffmpegBin
	default:
		return usagef("unknown -mode %q", *mode)
	}

	type job struct {
		path   string
		labels []bench.Occurrence
	}
	var todo []job
	for _, f := range set.Files {
		for _, p := range f.Paths {
			if strings.Contains(p, *only) {
				todo = append(todo, job{p, f.Ads})
			}
		}
	}
	runs := make([]bench.Run, len(todo))
	var runStore *store.Store // proxy runs: one copy of the ads, shared by the runs
	if *mode == "proxy" {
		if runStore, err = store.Copy(ctx, st.DB); err != nil {
			return err
		}
		defer runStore.Close()
	}
	parallel(len(todo), *jobs, func(i int) {
		j := todo[i]
		if *mode == "proxy" {
			runs[i] = bench.RunProxy(ctx, runStore, fmt.Sprintf("bench-%d", i), j.path, j.labels, opts)
		} else {
			runs[i] = bench.RunOffline(ctx, lib, j.path, j.labels, opts)
		}
		fmt.Fprintf(os.Stderr, "done %s (%.0fx)\n", filepath.Base(j.path), runs[i].Speed())
	})

	sum := bench.Summarize(runs)
	printBench(runs, sum, *mode, opts, *jobs, *verbose)
	if *jsonOut != "" {
		b, err := json.MarshalIndent(struct {
			Mode    string        `json:"mode"`
			Degrade string        `json:"degrade"`
			Summary bench.Summary `json:"summary"`
			Runs    []bench.Run   `json:"runs"`
		}{*mode, opts.Degrade.Name, sum, runs}, "", "  ")
		if err == nil {
			err = os.WriteFile(*jsonOut, b, 0o644)
		}
		if err != nil {
			return err
		}
	}
	if sum.Failed > 0 {
		return fmt.Errorf("%d of %d runs failed", sum.Failed, sum.Runs)
	}
	return nil
}

func degradeNames() string {
	var names []string
	for _, d := range bench.Degrades {
		names = append(names, d.Name)
	}
	return strings.Join(names, "|")
}

// parallel runs fn(0..n-1) on at most jobs goroutines.
func parallel(n, jobs int, fn func(i int)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, jobs)
	for i := range n {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn(i)
		}()
	}
	wg.Wait()
}

func printBench(runs []bench.Run, s bench.Summary, mode string, o bench.Options, jobs int, verbose bool) {
	fmt.Printf("%-44s %5s %5s %4s %6s %8s %6s\n", "file", "found", "conf", "fp", "noise", "weakest", "speed")
	for _, r := range runs {
		name := filepath.Base(r.Path)
		if r.Err != "" {
			fmt.Printf("%-44s ERROR %s\n", name, r.Err)
			continue
		}
		found, conf, weakest := 0, 0, 0
		for _, h := range r.Hits {
			if h.Found {
				found++
				if h.Det.Confirmed {
					conf++
				}
				if weakest == 0 || h.Det.Score < weakest {
					weakest = h.Det.Score
				}
			}
		}
		noise := "-"
		if r.Noise != nil {
			noise = fmt.Sprint(r.Noise.Score)
		}
		fmt.Printf("%-44s %2d/%-2d %5d %4d %6s %8d %5.0fx\n", name, found, len(r.Hits), conf, len(r.FalsePos), noise, weakest, r.Speed())
		for _, h := range r.Hits {
			if h.Found && !verbose {
				continue
			}
			line := fmt.Sprintf("    #%-3d %8.2f  ", h.Ad, h.Start.Seconds())
			if h.Found {
				line += fmt.Sprintf("found %+5d ms  score %4d", h.StartErr.Milliseconds(), h.Det.Score)
				if h.DetectAt != nil {
					line += fmt.Sprintf("  detect %+5.1fs", h.DetectAt.Seconds())
				}
				if h.ConfirmAt != nil {
					line += fmt.Sprintf("  confirm %+5.1fs", h.ConfirmAt.Seconds())
				} else if !h.Det.Confirmed {
					line += "  NOT CONFIRMED"
				}
			} else {
				line += "MISSED"
			}
			if !h.Verified {
				line += "  (unverified)"
			}
			fmt.Println(line)
		}
		for _, d := range r.FalsePos {
			fmt.Printf("    FP   %s %8.2f-%.2f  score %d\n", d.AdID.Short(), d.Start.Seconds(), d.End.Seconds(), d.Score)
		}
		if verbose && r.Noise != nil {
			fmt.Printf("    noise %s %8.2f  score %d\n", r.Noise.AdID.Short(), r.Noise.Start.Seconds(), r.Noise.Score)
		}
	}

	fmt.Println()
	fmt.Printf("mode %s", mode)
	if mode == "offline" {
		fmt.Printf(", degrade %s", o.Degrade.Name)
	}
	fmt.Printf(", minscore %d, confirm %d\n", o.MinScore, o.ConfirmScore)
	if s.Failed > 0 {
		fmt.Printf("FAILED runs: %d of %d\n", s.Failed, s.Runs)
	}
	fmt.Printf("recall          %.1f%% (%d/%d labelled occurrences", 100*s.Recall, s.Found, s.Labels)
	if s.Unverified > 0 {
		fmt.Printf(", %d unverified", s.Unverified)
	}
	fmt.Printf("), %d confirmed\n", s.Confirmed)
	perHour, hours := 0.0, s.Analysed.Hours()
	if hours > 0 {
		perHour = float64(s.FalsePos) / hours
	}
	fmt.Printf("false positives %d (%.2f/h over %.1f h)\n", s.FalsePos, perHour, hours)
	if s.StartErrN > 0 {
		fmt.Printf("boundary error  mean %.0f ms, max %.0f ms (%d manual labels)\n", float64(s.StartErrMeanAbs.Milliseconds()), float64(s.StartErrMaxAbs.Milliseconds()), s.StartErrN)
	} else {
		fmt.Printf("boundary error  not measured: no found occurrence has a manual label\n")
	}
	if s.StrongestNoise != nil {
		fmt.Printf("scores          weakest true %d, strongest noise %d (%s at %.2f in %s)",
			s.WeakestTrue, s.StrongestNoise.Score, s.StrongestNoise.AdID.Short(), s.StrongestNoise.Start.Seconds(), filepath.Base(s.NoisePath))
		if s.StrongestNoise.Score > 0 && s.WeakestTrue > 0 {
			fmt.Printf(", margin %.1fx", float64(s.WeakestTrue)/float64(s.StrongestNoise.Score))
		}
		fmt.Println()
	} else {
		fmt.Printf("scores          weakest true %d\n", s.WeakestTrue)
	}
	if s.DetectN > 0 {
		fmt.Printf("look-ahead      to detect: median %.1fs, max %.1fs; to confirm: median %.1fs, max %.1fs (%d)\n",
			s.DetectMedian.Seconds(), s.DetectMax.Seconds(), s.ConfirmMedian.Seconds(), s.ConfirmMax.Seconds(), s.ConfirmN)
	}
	fmt.Printf("speed           median %.0fx realtime per file (-j %d)\n", s.SpeedMedian, jobs)
}

// benchDraft writes a labels file listing what the detector finds in files, every entry
// unverified, for a person to check (and correct) before it is used as ground truth.
func benchDraft(ctx context.Context, out, dataDir string, files []string, minScore, jobs int) error {
	if len(files) == 0 {
		return usagef("-draft needs media files as arguments")
	}
	st, err := store.Open(ctx, dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	lib := st.Lib
	runs := make([]bench.Run, len(files))
	parallel(len(files), jobs, func(i int) {
		runs[i] = bench.RunOffline(ctx, lib, files[i], nil, bench.Options{MinScore: minScore})
		fmt.Fprintf(os.Stderr, "done %s\n", filepath.Base(files[i]))
	})
	dir, _ := filepath.Abs(filepath.Dir(out))
	rel := func(p string) string {
		abs, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		if r, err := filepath.Rel(dir, abs); err == nil {
			return r
		}
		return abs
	}
	set := bench.Set{DataDir: rel(dataDir)}
	for _, r := range runs {
		if r.Err != "" {
			slog.Warn("bench run failed", "path", r.Path, "err", r.Err)
			continue
		}
		f := bench.File{Paths: []string{rel(r.Path)}, Ads: []bench.Occurrence{}}
		for _, d := range r.FalsePos { // with no labels, every detection is "false"
			f.Ads = append(f.Ads, bench.Occurrence{Ad: d.AdID, Label: d.Label, Start: d.Start.Round(10 * time.Millisecond), End: d.End.Round(10 * time.Millisecond),
				Source: bench.SourceDetected, Note: fmt.Sprintf("score %d", d.Score)})
		}
		set.Files = append(set.Files, f)
	}
	b, err := json.MarshalIndent(set, "", "  ")
	if err == nil {
		err = os.WriteFile(out, append(b, '\n'), 0o644)
	}
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s: %d files; check every entry, then set \"verified\": true\n", out, len(set.Files))
	return nil
}
