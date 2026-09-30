package main

import (
	"crypto/rand"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
)

// runToken prints a new random token and nothing else: it touches no file. Put it under a
// user in auth.users and reload the config (SIGHUP, or the admin page's button) to make
// it valid.
func runToken(args []string) error {
	fl := flag.NewFlagSet("token", flag.ExitOnError)
	setupLog := logFlags(fl, slog.LevelWarn)
	if err := parse(fl, args, setupLog, false); err != nil {
		return err
	}
	fmt.Println(newToken())
	fmt.Fprintln(os.Stderr, "add it to tokens of a user in auth.users of the config file, then reload it: kill -HUP <adsvc pid>")
	return nil
}

// newToken is 256 random bits, base64url: letters, digits, '-' and '_', so it works as
// it is in NAME:TOKEN@host.
func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails: crypto/rand crashes the program instead
	return base64.RawURLEncoding.EncodeToString(b)
}
