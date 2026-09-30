package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

// logLevel is the level of the default logger; commands may change it after setup.
var logLevel slog.LevelVar

// logFlags registers -log-level and -log-format on fl, with defaults from $ADSVC_LOG_LEVEL
// (else def) and $ADSVC_LOG_FORMAT (else text). Call the returned function once fl has
// been parsed: it installs the default slog logger that every package logs to.
func logFlags(fl *flag.FlagSet, def slog.Level) func() error {
	lvl := new(slog.Level)
	*lvl = def
	var envErr error
	if env := os.Getenv("ADSVC_LOG_LEVEL"); env != "" {
		envErr = lvl.UnmarshalText([]byte(env))
	}
	fl.TextVar(lvl, "log-level", *lvl, "log level: debug, info, warn or error (also $ADSVC_LOG_LEVEL)")
	format := fl.String("log-format", envOr("ADSVC_LOG_FORMAT", "text"), "log format: text or json (also $ADSVC_LOG_FORMAT)")
	return func() error {
		if envErr != nil && !isSet(fl, "log-level") {
			return usagef("$ADSVC_LOG_LEVEL: %v", envErr)
		}
		logLevel.Set(*lvl)
		if err := installLog(*format); err != nil {
			return usagef("-log-format: %v", err)
		}
		return nil
	}
}

// installLog makes a logger in format (text or json) on stderr the default logger, at
// logLevel.
func installLog(format string) error {
	opts := &slog.HandlerOptions{Level: &logLevel}
	switch strings.ToLower(format) {
	case "text":
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, opts)))
	case "json":
		slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, opts)))
	default:
		return fmt.Errorf("log format %q: want text or json", format)
	}
	return nil
}

// parse parses args into fl and then sets up logging. Positional arguments are an error
// unless the command takes them.
func parse(fl *flag.FlagSet, args []string, setupLog func() error, positional bool) error {
	if err := fl.Parse(args); err != nil {
		return err
	}
	if fl.NArg() > 0 && !positional {
		return usagef("unexpected arguments: %s", strings.Join(fl.Args(), " "))
	}
	return setupLog()
}

// isSet reports whether the flag was given on the command line.
func isSet(fl *flag.FlagSet, name string) bool {
	set := false
	fl.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
