# STATUS — sovereign-proxy-daemon + browser-engine

Plain facts about what is verified and what is not. 2026-11-14.

## Verified by execution on this VM

- Go 1.23.12 builds both modules; `go vet` clean; `go test ./...` passes
  in both modules (proxy pool/manager/checker, server listener, cdp
  client, launcher flags).
- Daemon routes traffic: curl through SOCKS5 `127.0.0.1:1080` returned a
  real public IP through a working upstream.
- Per-session pinning (manager unit tests + live curl with a SOCKS5
  username) pins a session to one upstream; a dead pinned node is
  re-picked to a healthy one.
- Interval rotation changes the shared node after the interval (unit test).
- Health checker drops a dead SOCKS5 node within one 10 s round (live log).
- Fail-closed: with no healthy upstream the daemon refuses SOCKS5 CONNECT
  instead of dialing direct.
- TLS works through the tunnel (local self-signed TLS server fetched via
  the daemon, HTTP 200, body intact).
- Browser engine: real Chromium (Chrome/158) launched with the spec's
  flags; CDP over `--remote-debugging-pipe` (NUL-terminated JSON, measured
  by probe). All 11 fingerprint checks pass on the loaded page: iPhone
  user agent, platform iPhone, hardwareConcurrency 4, maxTouchPoints 5,
  webdriver absent, touch true, 390x844, devicePixelRatio 3, WebGL vendor
  "Apple Inc." / renderer "Apple GPU".
- Chromium + bare `socks5://127.0.0.1:1080` (no username) connects through
  the daemon: upstream dials for real hosts appear in the daemon log.

## NOT verified / known limits

- The external browserleaks.com audit did not complete on this VM. The
  sandbox egress proxy 403s some hosts and MITMs TLS with a certificate
  Chromium does not trust, so browserleaks.com rendered a certificate
  interstitial. The 11/11 checks above ran against that interstitial
  page; they prove the emulation values, not the audit site's own readout.
- Per-session binding via the SOCKS5 username does NOT work with
  Chromium: `socks5://<session>@127.0.0.1:1080` makes Chromium refuse the
  proxy outright (ERR_NO_SUPPORTED_PROXIES, daemon sees no connection).
  curl and other SOCKS5 clients accept it. Per-session mode for Chromium
  needs a different binding (per-session local ports, or HTTP proxy with
  Proxy-Authorization). Decision outstanding.
- **FIXED 2026-11-14 (owner-approved):** the checker used to treat
  `http://` upstreams as direct dials, marking a working HTTP upstream
  DOWN within one round so the daemon failed closed. `checker.go` now
  Live proof: with a working HTTP upstream + a dead node and a 10 s
  interval, after two checker rounds the log shows only the dead node
  DOWN; the working HTTP upstream stays up, and curl through the daemon
  still returns a public IP after the checks (before the fix it failed
  closed). Unit test `TestCheckNodeHTTPUpstreamCheckedThroughProxy`
  covers the healthy and dead cases against a local CONNECT stub.
- No real proxy pool is configured: the shipped config.yaml lists
  placeholder datacenter hosts. All live checks ran against local
  stand-ins (direct dial, a local stub SOCKS5 server, the sandbox egress
  proxy as a stand-in upstream). Real upstreams are the owner's to supply.
- Direct dialing to the public internet does not work from this VM: the
  sandbox only lets traffic out through its own egress proxy. A bare TLS
  ClientHello sent direct gets a non-TLS answer.
- The daemon has no TLS on its own listener and no authentication on the
  local port; it is a localhost-only tool by design (listen 127.0.0.1).

## Supply

- `go.mod` requires golang.org/x/net v0.24.0 and gopkg.in/yaml.v3 v3.0.1.
  The paste's own go.mod names them; they fetch fine. This is not a
  zero-dependency build, despite the spec's phrase.
- `config.go`, `pool.go`, `checker.go`, `config.yaml` (proxy sections) and
  the `cmd/daemon/main.go` structure are the supplied paste, verbatim.
  `manager.go`, `listener.go`, the listener wiring in main.go, and all of
  `browser-engine/` are new code written to complete the spec.
