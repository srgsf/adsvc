package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/srgsf/adsvc/internal/store"
)

// signalContext is the root context of a command. It ends on SIGINT or SIGTERM; every
// service the command starts (store, proxy, ffmpeg runs, HTTP requests) derives
// from it, so a signal makes each of them shut down on its own.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// defaultDataDir is the data directory when -data-dir names none. It is created when
// it does not exist (store.Open).
const defaultDataDir = "data"

// dataDirFlag is -data-dir with default def; "" leaves the default (defaultDataDir) to
// the caller, after the other layers (the config file).
func dataDirFlag(fl *flag.FlagSet, def string) *string {
	usage := "data directory: catalogue.db (ads, per-file ad maps) and tracking.csr (the ad index)"
	if def == "" {
		usage += " (default " + defaultDataDir + ")"
	}
	return fl.String("data-dir", def, usage)
}

// withStore opens the data directory dir under a root context of its own (signalContext),
// runs fn on it and closes it.
func withStore(dir string, fn func(context.Context, *store.Store) error) error {
	ctx, stop := signalContext()
	defer stop()
	st, err := store.Open(ctx, dir)
	if err != nil {
		return err
	}
	return errors.Join(fn(ctx, st), st.Close())
}
