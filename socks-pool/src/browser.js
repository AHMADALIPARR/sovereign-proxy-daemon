'use strict';
/**
 * ChromeSession: one headless Chromium, driven over DevTools, whose traffic
 * leaves through a proxy leased from a ProxyPool via a local SocksRelay.
 */
const { spawn } = require('node:child_process');
const { EventEmitter } = require('node:events');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { SocksRelay } = require('./relay');
const { CdpConnection, CdpClosedError } = require('./cdp');

class NavigationError extends Error {
  constructor(url, errorText) {
    super(`navigation to ${url} failed: ${errorText}`);
    this.name = 'NavigationError';
    this.url = url;
    this.errorText = errorText;
  }
}
class NavigationTimeoutError extends Error {
  constructor(url, ms) { super(`navigation to ${url} timed out after ${ms}ms`); this.name = 'NavigationTimeoutError'; }
}
class EvaluationError extends Error {
  constructor(text) { super(`page evaluation threw: ${text}`); this.name = 'EvaluationError'; }
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

function findChrome() {
  if (process.env.CHROME_PATH) return process.env.CHROME_PATH;
  const names = ['google-chrome', 'google-chrome-stable', 'chromium', 'chromium-browser', 'chrome'];
  for (const dir of (process.env.PATH ?? '').split(path.delimiter)) {
    for (const n of names) {
      const p = path.join(dir, n);
      if (fs.existsSync(p)) return p;
    }
  }
  throw new Error('no Chrome/Chromium found on PATH; set CHROME_PATH');
}

class ChromeSession extends EventEmitter {
  /**
   * @param {object} o
   * @param {import('./pool').ProxyPool} o.pool
   * @param {string} [o.key]                  sticky key for the pool
   * @param {string} [o.chromePath]
   * @param {boolean} [o.headless=true]
   * @param {string[]} [o.extraArgs]
   * @param {boolean} [o.proxyLoopback=false] also send localhost traffic through the proxy
   * @param {boolean} [o.sandbox]             default: off only when running as root
   * @param {number} [o.navigationTimeoutMs=30000]
   * @param {number} [o.launchTimeoutMs=15000]
   * @param {number} [o.connectTimeoutMs=10000] upstream SOCKS connect timeout
   */
  static async launch(o) {
    const s = new ChromeSession(o);
    try {
      await s.#start();
    } catch (err) {
      await s.close();
      throw err;
    }
    return s;
  }

  #o; #child = null; #exit = null; #stderr = ''; #spawnError = null;
  #exited; #resolveExited; #cdp = null; #sid = null; #relay = null; #dir = null; #closed = false;

  constructor(o) {
    super();
    this.#o = {
      headless: true, extraArgs: [], proxyLoopback: false, navigationTimeoutMs: 30000,
      launchTimeoutMs: 15000, connectTimeoutMs: 10000, ...o,
    };
    this.pool = o.pool;
    this.key = o.key;
    this.proxyFailures = 0;
    this.lease = null;
    this.#exited = new Promise((r) => { this.#resolveExited = r; });
  }

  /** Current exit proxy (credentials-free label via proxy.label). */
  get proxy() { return this.lease?.proxy; }
  get cdp() { return this.#cdp; }

  async #start() {
    const o = this.#o;
    this.lease = this.pool.acquire({ key: this.key });
    this.#relay = new SocksRelay({ upstream: this.lease.proxy, connectTimeoutMs: o.connectTimeoutMs });
    this.#relay.on('proxy-ok', ({ upstream, latencyMs }) => this.pool.reportSuccess(upstream, latencyMs));
    this.#relay.on('proxy-fail', ({ upstream, error }) => { this.proxyFailures++; this.pool.reportFailure(upstream, error); });
    this.#relay.on('client-error', (e) => this.emit('relay-error', e.error));
    this.#relay.on('server-error', (e) => this.emit('relay-error', e.error));
    await this.#relay.listen();

    this.#dir = fs.mkdtempSync(path.join(os.tmpdir(), 'socks-pool-'));
    const sandbox = o.sandbox ?? (process.getuid?.() !== 0);
    const args = [
      ...(o.headless ? ['--headless=new'] : []),
      '--remote-debugging-port=0',
      `--user-data-dir=${this.#dir}`,
      `--proxy-server=socks5://127.0.0.1:${this.#relay.port}`,
      ...(o.proxyLoopback ? ['--proxy-bypass-list=<-loopback>'] : []),
      // Keep WebRTC from revealing the real address via UDP outside the proxy.
      '--force-webrtc-ip-handling-policy=disable_non_proxied_udp',
      '--no-first-run', '--no-default-browser-check', '--disable-dev-shm-usage',
      // Chrome's own update/sync/telemetry chatter would otherwise burn proxy bandwidth and pollute exit logs.
      '--disable-background-networking', '--disable-component-update', '--disable-sync',
      '--disable-default-apps', '--no-pings',
      ...(sandbox ? [] : ['--no-sandbox']),
      ...o.extraArgs,
      'about:blank',
    ];
    this.#child = spawn(o.chromePath ?? findChrome(), args, { stdio: ['ignore', 'ignore', 'pipe'] });
    this.#child.stderr.on('data', (d) => { this.#stderr = (this.#stderr + d).slice(-4096); });
    this.#child.once('error', (err) => { this.#spawnError = err; this.#resolveExited(); });
    this.#child.once('exit', (code, signal) => { this.#exit = { code, signal }; this.#resolveExited(); });

    const wsUrl = await this.#waitForDevTools(o.launchTimeoutMs);
    this.#cdp = await CdpConnection.connect(wsUrl, { timeoutMs: o.connectTimeoutMs });
    const { targetId } = await this.#cdp.send('Target.createTarget', { url: 'about:blank' });
    ({ sessionId: this.#sid } = await this.#cdp.send('Target.attachToTarget', { targetId, flatten: true }));
    await Promise.all([
      this.#send('Page.enable'),
      this.#send('Network.enable'),
    ]);
  }

  async #waitForDevTools(timeoutMs) {
    const file = path.join(this.#dir, 'DevToolsActivePort');
    const deadline = Date.now() + timeoutMs;
    while (Date.now() < deadline) {
      if (this.#spawnError) throw new Error(`failed to start Chrome: ${this.#spawnError.message}`);
      if (this.#exit) {
        throw new Error(`Chrome exited during startup (code ${this.#exit.code}, signal ${this.#exit.signal}):\n${this.#stderr}`);
      }
      if (fs.existsSync(file)) {
        const [port, wsPath] = fs.readFileSync(file, 'utf8').split('\n');
        if (port && wsPath) return `ws://127.0.0.1:${port}${wsPath}`;
      }
      await sleep(50);
    }
    throw new Error(`Chrome did not expose DevTools within ${timeoutMs}ms:\n${this.#stderr}`);
  }

  #send(method, params) { return this.#cdp.send(method, params, { sessionId: this.#sid }); }

  /**
   * Navigate and wait for the load event.
   * @returns {Promise<{url: string, status: number|null}>}
   * @throws {NavigationError|NavigationTimeoutError}
   */
  async goto(url, { timeoutMs = this.#o.navigationTimeoutMs } = {}) {
    const docStatus = new Map(); // loaderId -> HTTP status
    const onEvent = (ev) => {
      if (ev.sessionId === this.#sid && ev.method === 'Network.responseReceived' && ev.params.type === 'Document') {
        docStatus.set(ev.params.loaderId, ev.params.response.status);
      }
    };
    this.#cdp.on('event', onEvent);
    const load = this.#cdp.waitFor('Page.loadEventFired', { sessionId: this.#sid, timeoutMs });
    try {
      const nav = await this.#send('Page.navigate', { url });
      if (nav.errorText) { load.cancel(); throw new NavigationError(url, nav.errorText); }
      if (!(await load.promise)) throw new NavigationTimeoutError(url, timeoutMs);
      return { url, status: docStatus.get(nav.loaderId) ?? null };
    } finally {
      load.cancel();
      this.#cdp.off('event', onEvent);
    }
  }

  /**
   * goto() that rotates the exit and retries when the proxy was at fault.
   * Failures not attributable to the proxy (DNS NXDOMAIN, target refused, etc.) are thrown immediately.
   */
  async navigateWithRotation(url, { attempts = 3, ...opts } = {}) {
    for (let i = 1; ; i++) {
      const before = this.proxyFailures;
      try {
        return await this.goto(url, opts);
      } catch (err) {
        const proxyAtFault = (err instanceof NavigationError || err instanceof NavigationTimeoutError)
          && this.proxyFailures > before;
        if (!proxyAtFault || i >= attempts) throw err;
        await this.rotate({ reason: `attempt ${i} failed: ${err.message}` });
      }
    }
  }

  /** Evaluate a JS expression in the page (awaits promises) and return its value. */
  async evaluate(expression) {
    const r = await this.#send('Runtime.evaluate', { expression, returnByValue: true, awaitPromise: true });
    if (r.exceptionDetails) {
      throw new EvaluationError(r.exceptionDetails.exception?.description ?? r.exceptionDetails.text);
    }
    return r.result.value;
  }

  /**
   * Switch to a different proxy without restarting Chrome. Open tunnels are dropped so the
   * next request leaves through the new exit. Throws PoolExhaustedError (leaving the current
   * exit in place) if no other proxy is available.
   */
  async rotate({ clearState = true, reason } = {}) {
    const old = this.lease;
    const next = this.pool.acquire({ key: this.key, exclude: [old.proxy.id] });
    this.lease = next;
    const dropped = this.#relay.setUpstream(next.proxy);
    old.release();
    if (clearState) { // cookies/cache from the old exit would link the identities
      await this.#send('Network.clearBrowserCookies');
      await this.#send('Network.clearBrowserCache');
    }
    this.emit('rotate', { from: old.proxy.label, to: next.proxy.label, droppedSockets: dropped, reason });
    return next.proxy;
  }

  async close() {
    if (this.#closed) return;
    this.#closed = true;
    if (this.#cdp && !this.#cdp.closed) {
      try {
        await this.#cdp.send('Browser.close', {}, { timeoutMs: 3000 });
      } catch (err) {
        if (!(err instanceof CdpClosedError)) throw err; // Chrome hanging up is the expected outcome
      }
    }
    if (this.#child && !this.#exit && !this.#spawnError) {
      const timedOut = await Promise.race([this.#exited.then(() => false), sleep(5000).then(() => true)]);
      if (timedOut) { this.#child.kill('SIGKILL'); await this.#exited; }
    }
    this.#cdp?.close();
    await this.#relay?.close();
    this.lease?.release();
    if (this.#dir) fs.rmSync(this.#dir, { recursive: true, force: true, maxRetries: 5, retryDelay: 100 });
  }
}

module.exports = { ChromeSession, NavigationError, NavigationTimeoutError, EvaluationError, findChrome };
