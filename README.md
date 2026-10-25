# wa-sender

Sends WhatsApp messages from a school's own number on behalf of a host program
(Jaris Desktop), using [whatsmeow](https://github.com/tulir/whatsmeow). It is a
separate program, licensed under the **GNU GPL v3** (see `LICENSE`), and talks to
its host only through the line-delimited JSON protocol in [`PROTOCOL.md`](PROTOCOL.md)
over stdin/stdout.

> wa-sender uses WhatsApp Web unofficially. WhatsApp may limit or ban a number
> that sends too many messages. Use a number dedicated to the school.

## Build

```bash
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=1.0.0 -X main.commit=$(git rev-parse HEAD)" .
./wa-sender --version
```

Every release on GitHub carries the binaries for Windows, Linux and macOS, this
source, and a feed signed by the Jaris vendor key (`index.json` +
`index.json.sig`, written by `cmd/mkindex`). Jaris Desktop installs and updates the
extension from that feed and runs a binary only when the signature and the binary's
SHA-256 match.

## Tests

```bash
go test -race ./...
WA_SENDER_LIVE=1 go test -run TestLivePairingQR ./...   # reaches WhatsApp, links nothing
```

## Release

1. `MIN_JARIS` holds the oldest Jaris Desktop version the release works with.
2. Tag `vX.Y.Z` and push; `.github/workflows/release.yml` builds, signs and
   publishes. Needs the `WA_EXT_SIGNING_KEY` secret (from `go run ./cmd/mkindex -genkey`)
   and, for Windows Authenticode, the `WIN_SIGN_SHA1` variable plus the `jaris-sign`
   runner.

`update-whatsmeow.yml` opens a weekly PR moving to the newest whatsmeow; `canary.yml`
checks every day that WhatsApp still accepts this version.
