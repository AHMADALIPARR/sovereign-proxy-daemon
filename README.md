# Sovereign Proxy Daemon + Mobile Chromium Engine

![socks-badge](assets/socks-badge.png)

A local, zero-trust browser and proxy stack written in Go. The daemon
multiplexes a pool of upstream proxies behind one local SOCKS5/HTTP
endpoint, health-checks the pool on a timer, and rotates upstreams
per session or on an interval. The browser engine launches a Chromium
instance through that endpoint and pins a mobile device profile onto
every page with the Chrome DevTools Protocol. Nothing in the stack
phones home, nothing runs in a cloud, and the daemon binds to
localhost only.

The stack has two halves with a clean seam between them: the proxy
daemon (`cmd/daemon`, `internal/`) routes traffic, and the browser
engine (`browser-engine/`) drives Chromium. Either half runs alone.
The daemon serves any SOCKS5 or HTTP client; the engine drives any
Chromium binary that speaks the DevTools pipe.

## Architecture

```
                          +------------------+
                          |  config.yaml     |
                          |  pool, rotation, |
                          |  health checks,  |
                          |  device profiles |
                          +--------+---------+
                                   |
              +--------------------+--------------------+
              |                                         |
   +------------------+                      +-------------------+
   |  proxy daemon    |                      |  browser engine   |
   |  127.0.0.1:1080  |<---- SOCKS5 ---------|  launcher         |
   |                  |     (or HTTP)        |  CDP over pipe    |
   |  listener        |                      |  mobile profile   |
   |  pool + manager  |                      +--------+----------+
   |  health checker  |                               |
   +--------+---------+                      Chromium (headless)
            |                               --proxy-server=...
            | routes through one            --disable-blink-
            | healthy upstream              features=...
            v                                |
   +------------------+                      | page reports:
   |  upstream pool   |                      | iPhone UA, 390x844,
   |  direct://       |                      | 4 cores, 5 touch
   |  socks5://       |                      | points, Apple GPU
   |  http(s)://      |
   +--------+---------+
            |
            v
        internet
```

The daemon is the only process that touches the network on behalf of
the browser. Chromium never dials an upstream directly; every
connection it opens goes to `127.0.0.1:1080`, and the daemon picks the
upstream, dials through it, and relays bytes in both directions. The
health checker runs on its own goroutine and only ever flips a node's
healthy flag — it never routes traffic and never writes to the pool's
rotation state.

## The proxy daemon

The daemon is `cmd/daemon/main.go` plus three packages under
`internal/`. It reads `config.yaml`, builds a pool from the upstream
list, starts the background health checker, starts the local
listener, and waits for a shutdown signal.

### The listener (`internal/server/listener.go`)

The listener accepts TCP connections on the configured address
(`127.0.0.1:1080` by default) and dispatches on the first byte. A
`0x05` first byte is a SOCKS5 handshake (RFC 1928); anything else is
treated as an HTTP proxy request.

SOCKS5 handling covers the greeting, method selection, and the
CONNECT command for IPv4, domain, and IPv6 destinations. Two
authentication methods are supported: no authentication, and
username/password. The username carries the session id: a client
connecting as `socks5://tab-7@127.0.0.1:1080` is speaking as session
`tab-7`, and the manager pins that session to one upstream for as
long as the node stays healthy. The password is accepted and ignored;
there is no password database, because the listener is localhost-only
and the username is a routing label, not a credential.

HTTP handling covers `CONNECT host:port` and absolute-form requests
(`GET http://...`). A CONNECT through a SOCKS5 or HTTP upstream is
tunnelled the same way a SOCKS5 CONNECT is: the daemon opens the
upstream, issues the upstream-side handshake, and then relays bytes
until one side closes.

Upstream dialing (`DialUpstream`) understands four schemes. `direct://`
dials the target straight out. `socks5://` and `socks4://` dial
through an upstream SOCKS proxy using the standard SOCKS5 handshake,
with optional `user:password` credentials taken from the URL.
`http://` and `https://` dial the upstream host and issue an HTTP
CONNECT for the target, sending `Proxy-Authorization: Basic` when the
URL carries credentials, and return the tunnelled connection once the
upstream answers `200`.

When no healthy upstream exists, the daemon fails closed: a SOCKS5
CONNECT is refused with an error reply and an HTTP request gets a
`502`, instead of the daemon silently dialling direct. A browser that
loses its whole pool stops loading pages rather than leaking traffic
around the proxy.

### The pool (`internal/proxy/pool.go`)

`ProxyPool` holds the upstream nodes and a round-robin cursor. Each
node carries an atomic healthy flag and an atomic latency value in
milliseconds. `Next()` returns the next healthy node in rotation, or
nil when the pool has no healthy node. `GetHealthyNodes()` returns the
healthy subset for inspection. Nodes start out assumed healthy; the
health checker is what earns or revokes that assumption.

### Rotation (`internal/proxy/manager.go`)

`Manager` implements the two rotation modes the configuration names.

Per-session mode binds a proxy to a session id. The first connection
carrying a session id picks the next healthy node and pins it; later
connections with the same id get the same node. A pinned node that
goes unhealthy is re-picked immediately on the next connection, so a
session never stays stuck to a dead proxy. Connections with no session
id fall through to the interval behaviour.

Interval mode keeps one shared node and rotates it when the node dies
or the configured interval expires. The interval is a duration, not a
request count, so a long-lived connection does not get its upstream
yanked mid-stream; rotation happens at the next connection after the
timer expires.

Weights are parsed from the configuration and carried on the node
records; the current rotation is round-robin over healthy nodes.

### Health checking (`internal/proxy/checker.go`)

The checker runs a batch every interval (10 seconds by default). Each
batch checks every node concurrently and writes the result into the
node's healthy flag and latency. The check is a GET against the
configured target URL (default `https://api.ipify.org`) with the
configured timeout (5 seconds by default). A 200 response marks the
node healthy and records the round-trip time; any error or non-200
response marks it unhealthy and logs the failure with the node's URL.

The check goes through the upstream itself, in the same way traffic
does. A SOCKS5 upstream is checked through a SOCKS5 dial; an HTTP
upstream is checked through a CONNECT tunnel. A direct node is
checked with a direct dial. This matters because a check that
bypassed the upstream would measure the wrong path and report a dead
proxy as healthy.

## The browser engine

The browser engine is a second Go module at `browser-engine/`. It
launches Chromium, applies a mobile device profile over the Chrome
DevTools Protocol, navigates, and prints a JSON report of what the
page actually sees.

### The launcher (`internal/launcher/launcher.go`)

The launcher builds the Chromium argument list, starts the process,
and hands the caller a CDP client. The flags come from the spec and
from the configuration:

- `--proxy-server=socks5://127.0.0.1:1080` routes all of Chromium's
  traffic through the daemon. When a session id is given, it is
  embedded as the SOCKS5 username (`socks5://tab-7@127.0.0.1:1080`).
- `--disable-setuid-sandbox` and `--disable-web-security` are the
  spec's networking flags.
- `--disable-features=IsolateOrigins,site-per-process,AudioServiceSandbox`
  is the spec's feature set.
- `--disable-blink-features=AutomationControlled` strips the
  `navigator.webdriver` flag from the page.
- `--force-webrtc-ip-handling-policy=disable_non_proxied_udp` forces
  WebRTC traffic through the proxy so STUN never leaks the local
  address.
- `--remote-debugging-pipe` opens the DevTools Protocol over file
  descriptors instead of a websocket.
- `--headless=new` runs headless; `--user-data-dir` points at a fresh
  temporary profile unless one is configured.

On this machine the launcher also appends `--no-sandbox` and
`--enable-unsafe-swiftshader`, which are environment necessities for
headless Chromium in a container without a GPU. They are not part of
the device profile.

The default Chromium binary is the Playwright build on this machine.
`browser.binary` in the configuration overrides it with an absolute
path to a custom build.

### CDP over the pipe (`internal/cdp/client.go`)

The client speaks the DevTools Protocol over Chromium's debugging
pipe. The pipe framing is NUL-terminated JSON: each message is a JSON
object followed by a `0x00` byte. The parent writes commands to the
child's file descriptor 3 and reads responses and events from file
descriptor 4. There is no websocket dependency and no third-party
library; the client is the Go standard library plus the JSON wire
format.

`Call` sends a method with an incrementing id and waits for the
response with the same id. A read loop splits the incoming stream on
NUL bytes, parses each message, and routes responses to the waiting
caller and events (method without id) to a channel. `WaitForEvent`
blocks for a named event on a named session, which is how the engine
waits for the page load event.

### The mobile profile (`internal/cdp/emulation.go`)

`ApplyMobileProfile` runs on every new target, in this order: enable
the Page and Runtime domains, register the spoof script to run before
any page script, override the user agent, override the device
metrics, and enable touch emulation.

The user agent override sets the mobile signature and the client
hints metadata together: platform, platform version, architecture,
device model, and the mobile flag, so the UA string and the hints
agree with each other instead of contradicting. The device metrics
override sets the viewport to 390 by 844 at a device scale factor of
3 with mobile enabled. Touch emulation is enabled with 5 maximum
touch points.

The spoof script is a JavaScript snippet injected with
`Page.addScriptToEvaluateOnNewDocument`, so it runs before every page
script. It pins `navigator.hardwareConcurrency` (4), `navigator.maxTouchPoints`
(5), `navigator.platform` (`iPhone`), `navigator.vendor` (`Apple
Computer, Inc.`), and removes `navigator.webdriver`. It also patches
`WebGLRenderingContext.prototype.getParameter` and the WebGL2
equivalent so the unmasked vendor and renderer read `Apple Inc.` and
`Apple GPU`. The snippet is page content by necessity — CDP injects
into the page's JavaScript context, and there is no other channel by
which a page can be made to report different navigator or WebGL
values. The engine itself is Go; the snippet is data it sends.

### The verification report (`main.go`)

After navigation, the engine evaluates one script on the page that
returns the fingerprint the page sees: user agent, platform,
hardware concurrency, maximum touch points, the webdriver property,
touch support, viewport size, device pixel ratio, and the WebGL
unmasked vendor and renderer. It compares each value against the
configured device profile and prints a JSON report with one entry per
check, each with the expected value, the actual value, and a pass
flag, plus an `all_pass` summary. The report also carries a snippet
of the page's visible text, so a runner can confirm what an external
audit page itself displayed. The process exits 0 when every check
passes and 1 otherwise.

## The JavaScript pool (`socks-pool/`)

A second implementation of the same stack in JavaScript, zero
dependencies, Node >= 22, at `socks-pool/`. It carries its own SOCKS5
client (`src/socks5.js`), a rotating proxy pool (`src/pool.js`), a
local relay (`src/relay.js`), a CDP client (`src/cdp.js`), and a
headless Chromium session (`src/browser.js`), with a CLI at
`bin/socks-pool.js` and its own test suite at `test/` (`npm test`,
22 tests).

The relay is how this half answers the Chromium session problem.
Chromium cannot authenticate to a SOCKS5 proxy and fixes its proxy
at launch, so Chromium points at the loopback relay with no auth;
the relay authenticates to the real upstream. `session.rotate()`
switches the relay's upstream live and drops open tunnels, so the
next request leaves through the new exit without restarting the
browser. Rotation also clears cookies and cache, so identities do
not link across exits.

The pool's strategies are round-robin, random, least-used, and
latency (EWMA; unmeasured proxies are tried first). A sticky key
pins a caller to one exit while it stays healthy. Failures count
against the proxy with exponential cooldown; after `maxFailures`
the proxy is dead and retried after `deadRetryMs`. Destination-side
SOCKS replies (3-6) are not held against the proxy and do not
trigger rotation. DNS is always resolved by the proxy. WebRTC is
restricted to proxied UDP. Credentials never appear in labels,
stats, errors, or CLI output.

Run it with a proxy list:

```sh
cd socks-pool
node bin/socks-pool.js --proxies proxies.txt --rotate-every 1 --eval 'document.title' https://example.com/
```

`npm test` runs the suite: the SOCKS5 client against an
independently written mock server, pool logic with an injected
clock, relay rotation, and end-to-end runs in real headless Chrome.

## Configuration

`config.yaml` is the single configuration file for both halves. The
daemon's loader reads the `server`, `health_check`, and
`upstream_proxies` sections and ignores the rest; the browser engine
reads the `browser` and `devices` sections.

```yaml
server:
  listen_addr: "127.0.0.1:1080"
  rotation_mode: "per_session" # options: per_session, interval
  rotation_interval_seconds: 30

health_check:
  target_url: "https://api.ipify.org"
  timeout_seconds: 5
  interval_seconds: 10

upstream_proxies:
  - url: "socks5://user:pass@proxy1.datacenter.com:1080"
    weight: 10
  - url: "http://user:pass@proxy2.datacenter.com:3128"
    weight: 5
  - url: "socks5://proxy3.datacenter.com:1080"
    weight: 8

browser:
  binary: "" # empty = Playwright Chromium on this machine
  proxy_server: "socks5://127.0.0.1:1080"
  extra_flags:
    - "--disable-setuid-sandbox"
    - "--disable-web-security"
    - "--disable-features=IsolateOrigins,site-per-process,AudioServiceSandbox"
    - "--disable-blink-features=AutomationControlled"
    - "--force-webrtc-ip-handling-policy=disable_non_proxied_udp"

devices:
  iphone17:
    user_agent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1"
    platform: "iPhone"
    width: 390
    height: 844
    device_scale_factor: 3
    mobile: true
    touch: true
    max_touch_points: 5
    hardware_concurrency: 4
    webgl_vendor: "Apple Inc."
    webgl_renderer: "Apple GPU"
```

`listen_addr` is the local endpoint clients point at. `rotation_mode`
selects per-session or interval rotation, and
`rotation_interval_seconds` sets the interval timer. `health_check`
sets the probe target, the per-probe timeout, and the cadence.
`upstream_proxies` is the pool: each entry is a URL with a scheme of
`direct`, `socks5`, `socks4`, `http`, or `https`, optional
`user:password` credentials in the URL, and a weight. `browser.binary`
is empty by default; `browser.proxy_server` is the endpoint the
launcher passes to Chromium; `browser.extra_flags` are appended to
every launch. `devices` maps profile names to their values; the
`iphone17` profile is the default.

`test-config.yaml` is a second configuration used by the test setup:
local stand-ins only (a direct node, a local stub SOCKS5 server, and
a dead node that never listens), so the suite verifies rotation and
failover without spending anything on a real proxy.

## Running it

Build both halves:

```sh
cd sovereign-proxy-daemon
go build -o /tmp/daemon ./cmd/daemon
cd browser-engine
go build -o /tmp/browser-engine .
```

Run the daemon from the repository root so it finds `config.yaml`:

```sh
/tmp/daemon
# [Daemon] Sovereign Proxy Multiplexer running on 127.0.0.1:1080
# [Daemon] Managing 3 upstream proxy nodes...
# [Server] listening on 127.0.0.1:1080 (mode=per_session)
```

Point any SOCKS5 client at it:

```sh
curl --proxy socks5h://127.0.0.1:1080 https://api.ipify.org
curl --proxy socks5h://tab-7@127.0.0.1:1080 https://api.ipify.org
```

The first command routes with no session; the second pins the
request to session `tab-7`. The daemon logs a line for every node
the checker drops and a line for every upstream dial that fails.

Run the browser engine against a URL:

```sh
/tmp/browser-engine --config ../config.yaml --profile iphone17 \
    --url https://browserleaks.com
```

The engine prints the JSON verification report and exits 0 when
every check passes. `--session` is optional; without it the browser
uses interval rotation. `--verify=false` skips the fingerprint
evaluation and only navigates.

One usage constraint: Chromium does not accept credentials in a
SOCKS5 proxy URL, so `--session` must be omitted when the engine
launches Chromium — the browser then routes with no session and the
daemon applies interval rotation. The `--session` flag exists for
SOCKS5 clients that do accept a username, such as curl, where the
username is how a session is pinned to one upstream.

## Adding a device profile

A profile is one entry under `devices:` in `config.yaml`, keyed by
the name passed to `--profile`. Each entry carries the user agent
string, the platform string, the viewport width and height, the
device scale factor, the mobile and touch flags, the maximum touch
points, the hardware concurrency, and the WebGL vendor and renderer
strings. The engine reads the entry at startup and applies every
field: the user agent and client hints come from the user agent and
platform fields, the viewport comes from the width, height, scale
factor, and mobile fields, and the spoof script takes the touch,
concurrency, platform, and WebGL fields. Adding a profile means
adding an entry; no code changes. The verification report compares
the page against the same entry, so a new profile is self-checking:
run the engine with `--profile` naming it and the report shows
whether the page sees the values the entry declares.

## Testing

The test suites live next to the code they cover. The daemon suite
covers pool rotation over healthy nodes, per-session pinning and
re-picking after a pin dies, interval rotation after the timer
expires, the health checker marking a local 200 target healthy and a
dead node unhealthy, SOCKS5 CONNECT through a direct upstream and
through a stub SOCKS5 upstream, a TLS page fetched through the
tunnel, and the fail-closed refusal with no healthy upstream. The
browser engine suite covers the flag builder (spec flags present, the
session username embedded only when a session is given, no proxy flag
when no proxy is configured), the CDP client's round-trip against a
fake Chromium, event routing, and the spoof script carrying the
profile's values.

```sh
go test ./...          # daemon module
go test ./...          # browser-engine module
```

## Design notes

The daemon is a multiplexer, not a proxy provider. It owns no exit
IPs and it does not find proxies; the pool is supplied in the
configuration and it is the operator's to fill. What the daemon owns
is the switching: which upstream a connection uses, how liveness is
measured, and what happens when the pool is empty.

Rotation is deliberately per connection, never mid-stream. A
connection that is already open keeps its upstream until it closes;
rotation decisions happen when a new connection arrives. Rotating an
IP underneath an open session breaks stateful connections, drops
cookies, and triggers bot-mitigation loops, so the daemon never does
it.

The local endpoint is a socket, not a service discovery mechanism.
Clients that need a session carry it in the SOCKS5 username, and
clients that do not need one get interval rotation. There is no
control API on the daemon and no authentication on the local port;
localhost is the trust boundary.

The browser engine's CDP client exists because the Go standard
library has no websocket client and the stack takes no dependencies
beyond what the configuration parser already requires. The debugging
pipe gives the same protocol over file descriptors, and the client
treats the pipe as a byte stream with NUL-delimited JSON messages.

`STATUS.md` at the repository root records the verification scope:
what has been run against this code, what the live checks showed, and
the limits that apply on this machine.

## License

This repository is licensed under the SNAPKITTY OPAQUE SOURCE
LICENSE v1.0. See `LICENSE` for the full text.
