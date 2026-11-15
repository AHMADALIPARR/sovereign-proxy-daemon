'use strict';
/**
 * Rotating SOCKS5 proxy pool: parsing, selection strategies, sticky keys,
 * failure cooldown with exponential backoff, circuit-breaker style "dead"
 * state with periodic retry, and active health checks.
 */
const fs = require('node:fs');
const { Socks5Error, connectViaSocks5, httpGetViaSocks5 } = require('./socks5');

class PoolExhaustedError extends Error {
  constructor(message) { super(message); this.name = 'PoolExhaustedError'; }
}
class ProxyParseError extends Error {
  constructor(message) { super(message); this.name = 'ProxyParseError'; }
}

/**
 * Accepts: socks5://user:pass@host:port, socks5h://host:port, host:port,
 *          host:port:user:pass, user:pass@host:port, [::1]-style IPv6 via URL form.
 */
function parseProxy(spec) {
  const s = spec.trim();
  let host; let port; let username; let password;
  if (s.includes('://')) {
    const u = new URL(s);
    if (u.protocol !== 'socks5:' && u.protocol !== 'socks5h:') {
      throw new ProxyParseError(`unsupported scheme "${u.protocol}" (need socks5:// or socks5h://)`);
    }
    host = u.hostname.replace(/^\[|\]$/g, '');
    port = u.port === '' ? 1080 : Number(u.port);
    username = u.username ? decodeURIComponent(u.username) : undefined;
    password = u.password ? decodeURIComponent(u.password) : undefined;
  } else if (s.includes('@')) {
    const at = s.lastIndexOf('@');
    const cred = s.slice(0, at);
    const addr = s.slice(at + 1).split(':');
    if (addr.length !== 2) throw new ProxyParseError('expected user:pass@host:port');
    const i = cred.indexOf(':');
    username = i < 0 ? cred : cred.slice(0, i);
    password = i < 0 ? '' : cred.slice(i + 1);
    [host, port] = [addr[0], Number(addr[1])];
  } else {
    const parts = s.split(':');
    if (parts.length === 2) [host, port] = [parts[0], Number(parts[1])];
    else if (parts.length === 4) [host, port, username, password] = [parts[0], Number(parts[1]), parts[2], parts[3]];
    else throw new ProxyParseError('expected host:port or host:port:user:pass');
  }
  if (!host) throw new ProxyParseError('missing host');
  if (!Number.isInteger(port) || port < 1 || port > 65535) throw new ProxyParseError(`bad port "${port}"`);
  return { host, port, username, password };
}

function parseProxyList(text) {
  return text.split(/\r?\n/)
    .map((line, i) => ({ line: line.trim(), n: i + 1 }))
    .filter(({ line }) => line && !line.startsWith('#'))
    .map(({ line, n }) => {
      try { return parseProxy(line); } catch (err) {
        if (err instanceof ProxyParseError || err instanceof TypeError) {
          throw new ProxyParseError(`line ${n}: ${err.message}`);
        }
        throw err;
      }
    });
}

class ProxyEntry {
  constructor({ host, port, username, password }, id) {
    this.id = String(id);
    this.host = host; this.port = port; this.username = username; this.password = password;
    this.uses = 0; this.inflight = 0; this.successes = 0;
    this.consecutiveFailures = 0; this.totalFailures = 0;
    this.latencyMs = null; this.cooldownUntil = 0; this.lastError = null;
  }
  /** Credentials are never part of the label. */
  get label() { return `${this.host}:${this.port}`; }
  toJSON() { return { id: this.id, label: this.label, authenticated: Boolean(this.username) }; }
}

const STRATEGIES = new Set(['round-robin', 'random', 'least-used', 'latency']);

class ProxyPool {
  #entries; #cursor = 0; #sticky = new Map(); #timer = null;

  /**
   * @param {Array<string|object>} specs proxy strings or {host,port,username,password}
   * @param {object} [opts]
   * @param {'round-robin'|'random'|'least-used'|'latency'} [opts.strategy='round-robin']
   * @param {number} [opts.maxFailures=3]       consecutive failures before "dead"
   * @param {number} [opts.cooldownBaseMs=5000] first cooldown; doubles per failure
   * @param {number} [opts.cooldownMaxMs=300000]
   * @param {number} [opts.deadRetryMs=60000]   dead proxies become selectable again after this
   * @param {number} [opts.maxPerProxy=Infinity] concurrent leases per proxy
   * @param {number} [opts.stickyTtlMs=600000]
   * @param {{host:string,port:number}} [opts.checkTarget] destination for health checks
   * @param {() => number} [opts.now]
   * @param {() => number} [opts.random]
   */
  constructor(specs, opts = {}) {
    const o = {
      strategy: 'round-robin', maxFailures: 3, cooldownBaseMs: 5000, cooldownMaxMs: 300000,
      deadRetryMs: 60000, maxPerProxy: Infinity, stickyTtlMs: 600000,
      checkTarget: { host: 'example.com', port: 443 }, now: Date.now, random: Math.random, ...opts,
    };
    if (!STRATEGIES.has(o.strategy)) throw new RangeError(`unknown strategy "${o.strategy}"`);
    if (specs.length === 0) throw new RangeError('proxy pool needs at least one proxy');
    this.opts = o;
    this.#entries = specs.map((s, i) => new ProxyEntry(typeof s === 'string' ? parseProxy(s) : s, i + 1));
  }

  static fromFile(path, opts) {
    return new ProxyPool(parseProxyList(fs.readFileSync(path, 'utf8')), opts);
  }

  get size() { return this.#entries.length; }

  #status(e, now) {
    if (e.cooldownUntil <= now) return 'healthy';
    return e.consecutiveFailures >= this.opts.maxFailures ? 'dead' : 'cooling';
  }

  #eligible(e, now, exclude) {
    return e.cooldownUntil <= now && e.inflight < this.opts.maxPerProxy && !exclude.has(e.id);
  }

  /**
   * Pick a proxy. Returns a lease; call lease.release() when done.
   * @param {{key?: string, exclude?: string[]}} [req] `key` pins a caller to one exit while it stays healthy.
   */
  acquire({ key, exclude = [] } = {}) {
    const now = this.opts.now();
    const ex = new Set(exclude);
    let chosen = null;

    if (key !== undefined) {
      const s = this.#sticky.get(key);
      if (s && s.expires > now) {
        const e = this.#entries.find((x) => x.id === s.id);
        if (e && this.#eligible(e, now, ex)) chosen = e;
      }
    }
    if (!chosen) {
      const candidates = this.#entries.filter((e) => this.#eligible(e, now, ex));
      if (candidates.length === 0) {
        const s = this.#entries.map((e) => `${e.label}=${this.#status(e, now)}`).join(', ');
        throw new PoolExhaustedError(`no proxy available (${s})`);
      }
      chosen = this.#select(candidates);
    }
    if (key !== undefined) this.#sticky.set(key, { id: chosen.id, expires: now + this.opts.stickyTtlMs });

    chosen.inflight++; chosen.uses++;
    let released = false;
    return {
      proxy: chosen,
      release: () => { if (!released) { released = true; chosen.inflight--; } },
    };
  }

  #select(candidates) {
    switch (this.opts.strategy) {
      case 'random':
        return candidates[Math.floor(this.opts.random() * candidates.length)];
      case 'least-used':
        return candidates.reduce((a, b) => (b.inflight < a.inflight || (b.inflight === a.inflight && b.uses < a.uses) ? b : a));
      case 'latency':
        // Unmeasured proxies sort first so they get explored.
        return candidates.reduce((a, b) => ((b.latencyMs ?? 0) < (a.latencyMs ?? 0) ? b : a));
      default: { // round-robin over the full list, skipping ineligible entries
        const set = new Set(candidates);
        for (let i = 0; i < this.#entries.length; i++) {
          const idx = (this.#cursor + i) % this.#entries.length;
          if (set.has(this.#entries[idx])) { this.#cursor = idx + 1; return this.#entries[idx]; }
        }
        throw new Error('unreachable: candidates not in entries');
      }
    }
  }

  reportSuccess(entry, latencyMs) {
    entry.successes++;
    entry.consecutiveFailures = 0;
    entry.cooldownUntil = 0;
    entry.lastError = null;
    if (latencyMs !== undefined) {
      entry.latencyMs = entry.latencyMs === null ? latencyMs : 0.7 * entry.latencyMs + 0.3 * latencyMs;
    }
  }

  /** Returns true if the failure was counted. Destination-side failures are not held against the proxy. */
  reportFailure(entry, err) {
    if (err instanceof Socks5Error && !err.blamesProxy) return false;
    const { maxFailures, cooldownBaseMs, cooldownMaxMs, deadRetryMs } = this.opts;
    entry.totalFailures++;
    const n = ++entry.consecutiveFailures;
    const cooldown = n >= maxFailures ? deadRetryMs : Math.min(cooldownBaseMs * 2 ** (n - 1), cooldownMaxMs);
    entry.cooldownUntil = this.opts.now() + cooldown;
    entry.lastError = err?.message ?? String(err);
    return true;
  }

  /** Connect-only health probe through one proxy. */
  async check(entry, { timeoutMs = 8000 } = {}) {
    const { host, port } = this.opts.checkTarget;
    try {
      const { socket, latencyMs } = await connectViaSocks5({ proxy: entry, host, port, timeoutMs });
      socket.destroy();
      this.reportSuccess(entry, latencyMs);
      return { id: entry.id, ok: true, latencyMs };
    } catch (error) {
      if (!(error instanceof Socks5Error)) throw error;
      const counted = this.reportFailure(entry, error);
      return { id: entry.id, ok: false, error: error.message, countedAgainstProxy: counted };
    }
  }

  async checkAll({ concurrency = 8, ...rest } = {}) {
    const queue = [...this.#entries];
    const results = [];
    const worker = async () => {
      for (let e = queue.shift(); e; e = queue.shift()) results.push(await this.check(e, rest));
    };
    await Promise.all(Array.from({ length: Math.min(concurrency, queue.length) }, worker));
    return results;
  }

  /** Fetch an http(s) URL through a specific proxy (e.g. an IP-echo endpoint) to learn its exit address. */
  probe(entry, url, opts) {
    return httpGetViaSocks5({ proxy: entry, url, ...opts });
  }

  startHealthLoop(intervalMs = 30000) {
    this.stopHealthLoop();
    this.#timer = setInterval(() => {
      this.checkAll().catch((error) => { throw error; }); // surface bugs as uncaught, never hide them
    }, intervalMs);
    this.#timer.unref();
  }
  stopHealthLoop() { clearInterval(this.#timer); this.#timer = null; }

  stats() {
    const now = this.opts.now();
    return this.#entries.map((e) => ({
      id: e.id, label: e.label, status: this.#status(e, now), uses: e.uses, inflight: e.inflight,
      successes: e.successes, failures: e.totalFailures, consecutiveFailures: e.consecutiveFailures,
      latencyMs: e.latencyMs === null ? null : Math.round(e.latencyMs),
      cooldownMs: Math.max(0, e.cooldownUntil - now), lastError: e.lastError,
    }));
  }
}

module.exports = { ProxyPool, PoolExhaustedError, ProxyParseError, parseProxy, parseProxyList };
