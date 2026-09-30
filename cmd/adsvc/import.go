package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"

	"github.com/srgsf/adsvc/internal/store"
)

// runImport ingests snapshots (every record checked as if a peer had sent it).
func runImport(args []string) error {
	fl := flag.NewFlagSet("import", flag.ExitOnError)
	dir := dataDirFlag(fl, defaultDataDir)
	setupLog := logFlags(fl, slog.LevelInfo)
	if err := parse(fl, args, setupLog, true); err != nil {
		return err
	}
	if fl.NArg() == 0 {
		return usagef("need snapshot files")
	}
	return withStore(*dir, func(ctx context.Context, st *store.Store) error {
		var errs []error
		for _, p := range fl.Args() {
			res, err := st.DB.ImportSnapshot(ctx, p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p, err))
				continue
			}
			fmt.Printf("%s: %d accepted, %d already known, rejected %v\n", p, res.Accepted, res.Duplicate, res.Rejected)
		}
		if err := st.Lib.Refresh(ctx); err != nil {
			errs = append(errs, err)
		}
		return errors.Join(errs...)
	})
}
