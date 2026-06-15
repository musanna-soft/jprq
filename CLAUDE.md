# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

jprq exposes a local server to the public internet over a tunnel (like ngrok) —
HTTP via a subdomain, or raw TCP via a reserved public port. This repo is a
**musanna-soft fork of `azimjohn/jprq`**: the Go module path stays
`github.com/azimjohn/jprq` (imports use it), but several behaviours diverge from
upstream. Every divergence is marked with a `FORK PATCH (musanna-soft)` comment —
grep for it before assuming upstream behaviour. The big ones:

- **Auth** is musanna-platform PAT validation, not GitHub OAuth (`server/musanna/`).
- **TLS** can be delegated to a k8s ingress (`JPRQ_PUBLIC_TLS_PORT=0` turns the
  in-process TLS listener off).
- The server **embeds and serves a small website** (`server/static/`) on the base domain.
- `GET /healthz` short-circuits to `200` regardless of Host (kubelet probe).

## Two binaries, one module

- **Server** — `server/` (`package main`). Long-running daemon. Built by the
  `Dockerfile` (`go build ./server/`), a single static binary with the website embedded.
- **CLI** — `cli/` (`package main`). The client users run. Cross-compiled for all
  platforms by `build.sh` into `bin/`.

They share no Go packages directly; they talk over the framed event protocol
(`server/events/`). `cli/debugger/` is the local request inspector (`--debug`).

## Commands

```bash
# Build the CLI for every target → bin/
./build.sh

# Build / run the server (needs env, JPRQ_DOMAIN at minimum — see Config)
go run ./server
go build ./server/

# Format + import check (CI gate)
make fmt

# Lint
make verify          # golangci-lint --config tools/.golangci.yaml ./...

# Test (go vet + go test). NOTE the -skip:
make test            # go test ./... -skip TestConfig_Load

# A single test
go test ./server/config -run TestConfig_Load
go test ./server/server -run TestTCPServer
```

**`TestConfig_Load` is skipped by `make test`** because it mutates process env;
a bare `go test ./...` will run (and can flake on) it. Use `make test` or target
packages individually.

## Server architecture

`server/main.go` loads `config`, builds the `musanna` authenticator, and starts
`Jprq` (`server/jprq.go`) plus the embedded website goroutine. `Jprq` runs up to
three `server.TCPServer` listeners:

| Listener | Default port | Role |
|---|---|---|
| event server | `JPRQ_EVENT_PORT` 4321 | CLI control channel (one long-lived conn per tunnel) |
| public server | `JPRQ_PUBLIC_PORT` 80 | inbound visitor traffic, routed by `Host` header |
| public TLS | `JPRQ_PUBLIC_TLS_PORT` 443 | same, with TLS — **disabled when set to 0** (ingress terminates) |

**Tunnel lifecycle** (`serveEventConn` in `jprq.go`) is the core flow: receive
`MsgTunnelRequested` → authenticate the PAT → enforce `MaxTunnelsPerUser` →
resolve + `validate()` the subdomain → register the tunnel in the right map →
reply `MsgTunnelOpened` → pump multiplexed frames until the conn drops. All four
tunnel maps (`httpTunnels`, `tcpTunnels`, `userTunnels`, `cnameMap`) are guarded
by the single `Jprq.mu`; every map write is bracketed by lock/unlock and a
deferred delete on teardown — keep that discipline when adding tunnel state.

**Routing** (`servePublicConn`): parse the `Host` header from the first bytes,
map a CNAME to its tunnel host via `cnameMap`, then look up `httpTunnels[host]`.
Base domain (`DomainName` / `www.`) is reverse-proxied to the embedded website on
`127.0.0.1:JPRQ_WEBSITE_PORT` (3300). Visitor↔CLI bytes are multiplexed as
`MsgConnectionData` / `MsgConnectionClose` frames keyed by `StreamID`.

**Keepalive matters here.** The control conn is long-lived; both SO_KEEPALIVE
(15 s) and an app-level `MsgPing` every 10 s exist specifically so stateful NAT /
conntrack doesn't silently evict the tunnel. Don't remove either without
understanding the `tunnel-closed`-every-20s symptom they fix.

**TCP tunnels** may request a fixed public port, but only inside the reserved
range checked in `jprq.go` (33000–33009) so users can't squat on 22/80/443/4321.

## Auth (fork patch)

`server/musanna/musanna.go` implements `Authenticator.Authenticate(token)` by
POSTing `{"token": …}` to `…/api/keys/validate?appId=<MUSANNA_APP_ID>` and
decoding the owning `User`. The PascalCase JSON tags mirror musanna-platform's
MVC controllers; `User.Login` (lowercased) is the identity used everywhere
downstream (default subdomain, per-user tunnel limit, log lines). Endpoint
precedence: `MUSANNA_VALIDATE_URL` → `MUSANNA_BASE_URL` + `/api/keys/validate` →
`https://platform.musanna.uz/api/keys/validate`. `MUSANNA_APP_ID` is required for
the scope check.

## Subdomain rules

`server/utils.go` `validate()`: lowercase alphanumeric + hyphen, 3–38 chars,
not in `blockList` (`www`, `jprq`). An invalid name is run through `sanitize()`
once before failing. Empty subdomain defaults to `user.Login` (`jprq.go`).

## Subdomain moderation (fork patch)

`server/moderation/` screens the resolved subdomain against the **shirinsoz**
service right after `validate()` succeeds in `serveEventConn` — so a tunnel can't
be opened on a profane custom name. `Guard.IsProfane(name)` mints a
`client_credentials` token (cached) and POSTs to shirinsoz `/api/shirinsoz/scan`
(strict categories + `crossLanguage`). **Fail-open**: disabled/unconfigured/
unreachable shirinsoz → `false` (allow), never blocks tunnel creation. Off unless
`MODERATION_ENABLED=true`; needs a confidential `jprq-svc` client on the platform
with the `shirinsoz.internal` scope (the validate-only PAT path can't mint it).
Env: `MODERATION_ENABLED`, `SHIRINSOZ_BASE_URL`, `MODERATION_TOKEN_URL`
(default `MUSANNA_BASE_URL` + `/connect/token`), `MODERATION_CLIENT_ID`,
`MODERATION_CLIENT_SECRET`, `MODERATION_SCOPE` (default `shirinsoz.internal`).

## Config / env

`server/config/config.go` `Load()`. `JPRQ_DOMAIN` is **required**. `MaxTunnelsPerUser`
(4) and `MaxConsPerTunnel` (24) are hardcoded, not env. TLS cert/key
(`JPRQ_TLS_CERT` / `JPRQ_TLS_KEY`) are required only when the TLS port ≠ 0.
`JPRQ_WEBSITE_PORT` (3300) is read in `main.go`, not in `Config`.

Deployment: `Dockerfile` (static binary, `EXPOSE 80 443 4321 3300`) is the
current target; `jprq.service` is a legacy systemd unit that still references the
pre-fork GitHub OAuth env and a `jprq-server` path — treat it as stale.
