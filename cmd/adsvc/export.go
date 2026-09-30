package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/srgsf/adsvc/internal/store"
)

// runExport writes a snapshot: every record of the catalogue, as a file anyone may have.
func runExport(args []string) error {
	fl := flag.NewFlagSet("export", flag.ExitOnError)
	dir := dataDirFlag(fl, defaultDataDir)
	out := fl.String("o", "", "snapshot file to write (must not exist)")
	setupLog := logFlags(fl, slog.LevelInfo)
	if err := parse(fl, args, setupLog, false); err != nil {
		return err
	}
	if *out == "" {
		return usagef("need -o snapshot.db")
	}
	return withStore(*dir, func(ctx context.Context, st *store.Store) error {
		if err := st.DB.Snapshot(ctx, *out); err != nil {
			return err
		}
		h, _ := st.DB.Head(ctx)
		fmt.Printf("wrote %s: %d records\n", *out, h)
		return nil
	})
}
