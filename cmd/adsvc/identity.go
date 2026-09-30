package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"

	"github.com/srgsf/adsvc/internal/store"
)

// runIdentity prints this node's id (what peers list in trust.origins).
func runIdentity(args []string) error {
	fl := flag.NewFlagSet("identity", flag.ExitOnError)
	dir := dataDirFlag(fl, defaultDataDir)
	setupLog := logFlags(fl, slog.LevelWarn)
	if err := parse(fl, args, setupLog, false); err != nil {
		return err
	}
	return withStore(*dir, func(_ context.Context, st *store.Store) error {
		fmt.Println(st.DB.Self())
		return nil
	})
}
