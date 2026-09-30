// Command adsgen generates a synthetic federation testbed: snapshots of made-up origins
// and configured, seeded node directories (see internal/testdb and docs/federation-testbed.md).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/srgsf/adsvc/internal/testdb"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "adsgen:", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	var o testdb.Options
	fl := flag.NewFlagSet("adsgen", flag.ExitOnError)
	out := fl.String("o", "testbed", "output directory (replaced)")
	fl.Uint64Var(&o.Seed, "seed", 1, "random seed: the same seed gives the same ads, files and votes")
	fl.IntVar(&o.Honest, "honest", 0, "honest origins (3)")
	fl.IntVar(&o.Ads, "ads", 0, "honest ads in all (900)")
	fl.IntVar(&o.Files, "files", 0, "files in the shared pool (2500)")
	fl.IntVar(&o.Collisions, "collisions", 0, "honest ads the collider copies (60)")
	fl.IntVar(&o.TrashAds, "trash-ads", 0, "ads the trash origin publishes (3500)")
	fl.IntVar(&o.TrashMaps, "trash-maps", 0, "file maps the trash origin publishes (1800; ads+maps over 5000 exceed the default daily quota)")
	fl.IntVar(&o.Days, "days", 0, "how far back the records reach (45)")
	hosts := fl.String("hosts", "", "node=host,… where nodes are reached (default localhost), e.g. hub=192.168.1.10,mirror=192.168.1.11")
	seed := fl.Bool("import", true, "import each node's snapshots into its data directory")
	_ = fl.Parse(os.Args[1:])

	hm := map[string]string{}
	for kv := range strings.SplitSeq(*hosts, ",") {
		if kv == "" {
			continue
		}
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return fmt.Errorf("-hosts %q: want node=host", kv)
		}
		hm[k] = v
	}
	w, err := testdb.Generate(o)
	if err != nil {
		return err
	}
	l, err := testdb.Build(ctx, w, *out, hm, *seed)
	if err != nil {
		return err
	}
	fmt.Println(l.Plan(w))
	return nil
}
