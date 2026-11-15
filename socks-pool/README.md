# socks-pool

Rotating SOCKS5 proxy pool that drives headless Chromium over the DevTools Protocol.
Zero dependencies; needs Node ≥ 22 (built-in `WebSocket`) and a Chrome/Chromium binary.

```
 your code ──► ProxyPool ──lease──► ChromeSession ──CDP/ws──► headless Chrome
                  ▲  health, cooldown,        │                     │ socks5://127.0.0.1:<port>
                  │  strategies, sticky       ▼                     ▼
                  └──── proxy-ok/fail ◄── SocksRelay ──(auth, remote DNS)──► upstream SOCKS5 ──► internet
```

## Why a local relay
Chromium cannot authenticate to SOCKS5 proxies and fixes its proxy at launch. Chrome therefore talks to a
loopback no-auth relay; the relay authenticates to the real upstream and can **switch upstream live**
(`session.rotate()`) without restarting the browser. Rotation also drops open tunnels, so keep-alive
connections can't continue exiting through the old IP.

## Use
```js
const { ProxyPool, withSession } = require('socks-pool');

const pool = ProxyPool.fromFile('proxies.txt', { strategy: 'latency', maxFailures: 3 });
pool.startHealthLoop(30_000);

await withSession({ pool }, async (page) => {
  await page.navigateWithRotation('https://example.com/', { attempts: 3 }); // rotates on proxy failure
  console.log(await page.evaluate('document.title'), page.proxy.label);
  await page.rotate();                                                      // new exit, cookies/cache cleared
  await page.goto('https://example.com/other');
});
```
Proxy formats (one per line, `#` comments): `socks5://user:pass@host:port`, `host:port`,
`host:port:user:pass`, `user:pass@host:port`.

CLI: `node bin/socks-pool.js --proxies proxies.txt --rotate-every 1 --eval 'document.title' URL...`

## Behaviour worth knowing
- **Strategies:** `round-robin`, `random`, `least-used`, `latency` (EWMA; unmeasured proxies are tried first). `key` pins a caller to one exit (sticky) until it is unhealthy or excluded.
- **Failure model:** consecutive failures → exponential cooldown → after `maxFailures` the proxy is "dead" and retried after `deadRetryMs` (half-open). Failures that are the *destination's* fault (SOCKS replies 3–6) are not held against the proxy and are not retried by rotating.
- **DNS** is always resolved by the proxy (hostnames are sent as SOCKS5 DOMAIN addresses).
- **WebRTC** is restricted to proxied paths (`disable_non_proxied_udp`) so UDP can't reveal your address.
- **Localhost** bypasses the proxy by default; set `proxyLoopback: true` to route it too (used by the tests).
- `rotate()` throws `PoolExhaustedError` and leaves the current exit untouched if no other proxy is available.
- Credentials never appear in labels, stats, errors or CLI output.

## Limits / caveats
- The relay listens on `127.0.0.1` with **no authentication** (Chrome can't supply any). Any local process can use it; don't run this on a shared host you don't trust.
- UDP/QUIC and SOCKS5 `UDP ASSOCIATE`/`BIND` are not supported (CONNECT only).
- Chrome may still make a few background requests at startup (e.g. Google update/accounts hosts) despite the `--disable-*` flags. They go through the proxy (no leak) but show up in upstream logs; block them with `extraArgs: ['--host-resolver-rules=MAP update.googleapis.com ~NOTFOUND, ...']` if that matters.
- Exit-IP verification is not automatic: use `pool.probe(entry, 'https://<ip-echo>')` or navigate to an echo URL.
- Use only proxies you're authorized to use, and respect the terms of the sites you visit.

## Tests
`npm test` — 22 tests: SOCKS5 client against an independently written mock server (auth, remote DNS, timeouts,
error classification), pool logic with an injected clock, relay rotation, and end-to-end runs in real headless Chrome
(remote DNS, rotation switching exits, cookie clearing, failover from a dead proxy, no rotation on destination failures).
