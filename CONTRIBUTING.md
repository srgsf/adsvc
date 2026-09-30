# Contributing

Thanks for helping. Bug reports, fixes, docs and ideas are all welcome.

## Before you start

- For anything bigger than a small fix, open an issue first so we can agree on the approach.
- Security problems: do not open an issue, see [SECURITY.md](SECURITY.md).
- adsvc only **finds and reports** ads; acting on a report is the player's business. Changes
  should keep that split.

## Development

You need `make` and a container runtime (Docker or Apple `container`). Everything else runs
in containers; see `make help` for all targets.

```sh
make test      # go test -race, with a full ffmpeg as the oracle
make vet       # go vet + gofmt
make lint      # golangci-lint
make build     # dist/adsvc
```

Code conventions, architecture and the reasons behind them are in the docs:
[docs/usage.md](docs/usage.md) (users), [docs/handoff.md](docs/handoff.md) (player
integration) and [docs/database.md](docs/database.md) (design).

## Guidelines

- Pure Go, no CGO. ffmpeg is used only as an external binary.
- **No new dependencies** without discussing it in an issue first. There are only two direct
  modules on purpose; prefer the standard library.
- Keep `go vet`, `gofmt` and `golangci-lint` clean.
- Add tests. Tests generate their own media with ffmpeg (`internal/testmedia`) and skip when
  ffmpeg is missing; parsers of untrusted bytes also get a fuzz target.
- Times are `time.Duration` in Go and integer milliseconds on the wire and on disk.
- Logging is `log/slog` with the default logger; never log raw upstream URLs, tokens or file
  names.
- User-facing changes update `docs/usage.md` (and `docs/handoff.md` for the client API).

## Pull requests

1. Fork, and branch from `main`.
2. Keep each pull request to one change, with a message that says why.
3. Make sure `make vet lint test` pass; CI runs the same.
4. Fill in the pull request template.

By contributing you agree that your work is licensed under the [MIT License](LICENSE).
