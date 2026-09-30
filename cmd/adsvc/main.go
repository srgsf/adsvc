// Command adsvc detects known ads in video streams by audio fingerprinting: a pass-through
// proxy for players, plus offline tools. Ads are marked by players through /ads/mark (or
// reported with /ads/report and marked later in mpv), and listed and removed in the web
// page at the root.
//
//	adsvc [proxy] -listen 127.0.0.1:8080 -data-dir data [-config config.yml]
//	              [-route https://ts.example.com=http://127.0.0.1:8090]
//	adsvc scan   -in <file|url> [-ffmpeg /path/to/ffmpeg]
//	adsvc bench  -labels bench/labels.json [-mode offline|proxy] [-degrade aac48]
//	adsvc token                                   (a new token: add it to auth.users, then SIGHUP)
//	adsvc identity                                (this node's id, for peers' trust.origins)
//	adsvc export -o snapshot.db                   (every record, a file anyone may have)
//	adsvc import snapshot.db...                   (records checked as if a peer sent them)
//
// The data directory (-data-dir, default ./data, created when missing) holds catalogue.db
// (SQLite: ads and per-file ad maps), tracking.csr (the ad index, rebuilt from the
// catalogue when needed) and identity.key.
//
// proxy is the default: a command line without a command, or one starting with a flag, runs
// it.
//
// Every command takes -log-level (debug, info, warn, error) and -log-format (text, json);
// $ADSVC_LOG_LEVEL and $ADSVC_LOG_FORMAT set their defaults. Logs go to stderr, results
// to stdout.
package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// version is set at release time by goreleaser (-X main.version=...).
var version = "dev"

// command is a subcommand.
type command struct {
	name, summary string
	run           func(args []string) error
}

// commands are the subcommands, in the order the help lists them.
var commands = []command{
	{"proxy", "the pass-through proxy players stream through (the default command)", runProxy},
	{"scan", "detect ads in a whole file or URL, decoded by ffmpeg", runScan},
	{"bench", "labelled benchmark: recall, false positives, look-ahead", runBench},
	{"token", "print a new random token for auth.users (touches no file)", runToken},
	{"identity", "print this node's id, for peers' trust.origins", runIdentity},
	{"export", "write every record to a snapshot file anyone may have", runExport},
	{"import", "read snapshot files, checked as if a peer sent them", runImport},
}

func lookup(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func main() {
	os.Exit(run(os.Args[1:]))
}

// run executes a command line and returns the exit code: 0 on success, 1 when the command
// failed, 2 for a usage error.
func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			fmt.Printf("adsvc %s\n", version)
			return 0
		case "help", "-h", "-help", "--help":
			if len(args) > 1 { // help <command>: its flags
				if _, ok := lookup(args[1]); ok {
					return run([]string{args[1], "-h"})
				}
				fmt.Fprintf(os.Stderr, "adsvc: unknown command %q\n\n", args[1])
				usage(os.Stderr)
				return 2
			}
			usage(os.Stdout)
			return 0
		}
	}
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		args = append([]string{"proxy"}, args...)
	}
	cmd, ok := lookup(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "adsvc: unknown command %q\n\n", args[0])
		usage(os.Stderr)
		return 2
	}
	err := cmd.run(args[1:])
	var uerr usageError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &uerr):
		fmt.Fprintf(os.Stderr, "adsvc %s: %v (-h for help)\n", args[0], err)
		return 2
	default:
		slog.Error("command failed", "command", args[0], "err", err)
		return 1
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, "adsvc %s detects known ads in video streams by audio fingerprinting.\n\n", version)
	fmt.Fprintln(w, "Usage:\n  adsvc [command] [flags]\n\nCommands:")
	for _, c := range commands {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprintf(w, "  %-9s %s\n", "version", "print the version")
	fmt.Fprintln(w, "\nWithout a command, or with flags only, adsvc runs proxy.")
	fmt.Fprintln(w, "Ads are marked from the player (/ads/mark, or /ads/report and later in mpv) and managed in the web page (/).")
	fmt.Fprintln(w, "\n`adsvc help <command>` or `adsvc <command> -h` lists a command's flags.")
}

// usageError is a command line that cannot work, as opposed to a command that failed.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return usageError{fmt.Sprintf(format, args...)}
}
