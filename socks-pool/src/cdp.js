'use strict';
/** Minimal Chrome DevTools Protocol client over Node's built-in WebSocket. */
const { EventEmitter } = require('node:events');

class CdpError extends Error {
  constructor(method, { code, message }) {
    super(`${method}: ${message} (${code})`);
    this.name = 'CdpError';
    this.code = code;
  }
}
class CdpClosedError extends Error {
  constructor(message = 'DevTools connection closed') { super(message); this.name = 'CdpClosedError'; }
}
class CdpTimeoutError extends Error {
  constructor(method, ms) { super(`${method} timed out after ${ms}ms`); this.name = 'CdpTimeoutError'; }
}

/** Emits 'event' {method, params, sessionId} for every protocol event, and 'close'. */
class CdpConnection extends EventEmitter {
  #ws; #nextId = 1; #pending = new Map(); #closed = false;

  static async connect(url, { timeoutMs = 10000 } = {}) {
    const ws = new WebSocket(url);
    await new Promise((resolve, reject) => {
      const timer = setTimeout(() => { ws.close(); reject(new Error(`timed out connecting to ${url}`)); }, timeoutMs);
      ws.addEventListener('open', () => { clearTimeout(timer); resolve(); }, { once: true });
      ws.addEventListener('error', (e) => {
        clearTimeout(timer);
        reject(new Error(`websocket error connecting to ${url}: ${e.message ?? 'unknown'}`));
      }, { once: true });
    });
    return new CdpConnection(ws);
  }

  constructor(ws) {
    super();
    this.#ws = ws;
    ws.addEventListener('message', (ev) => this.#onMessage(JSON.parse(ev.data)));
    ws.addEventListener('close', () => this.#onClose());
  }

  get closed() { return this.#closed; }

  send(method, params = {}, { sessionId, timeoutMs = 30000 } = {}) {
    if (this.#closed) return Promise.reject(new CdpClosedError());
    const id = this.#nextId++;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        this.#pending.delete(id);
        reject(new CdpTimeoutError(method, timeoutMs));
      }, timeoutMs);
      this.#pending.set(id, { method, resolve, reject, timer });
      this.#ws.send(JSON.stringify({ id, method, params, ...(sessionId && { sessionId }) }));
    });
  }

  /**
   * Wait for one event. Never rejects: resolves true on event, false on timeout.
   * `cancel()` detaches without settling.
   */
  waitFor(method, { sessionId, timeoutMs = 30000 } = {}) {
    let cancel;
    const promise = new Promise((resolve) => {
      const onEvent = (ev) => {
        if (ev.method === method && (sessionId === undefined || ev.sessionId === sessionId)) { done(true); }
      };
      const timer = setTimeout(() => done(false), timeoutMs);
      const done = (v) => { clearTimeout(timer); this.off('event', onEvent); resolve(v); };
      cancel = () => { clearTimeout(timer); this.off('event', onEvent); };
      this.on('event', onEvent);
    });
    return { promise, cancel };
  }

  close() { if (!this.#closed) this.#ws.close(); }

  #onMessage(msg) {
    if (msg.id === undefined) {
      this.emit('event', { method: msg.method, params: msg.params, sessionId: msg.sessionId });
      return;
    }
    const p = this.#pending.get(msg.id);
    if (!p) return; // response to a request that already timed out
    this.#pending.delete(msg.id);
    clearTimeout(p.timer);
    if (msg.error) p.reject(new CdpError(p.method, msg.error)); else p.resolve(msg.result);
  }

  #onClose() {
    this.#closed = true;
    for (const p of this.#pending.values()) { clearTimeout(p.timer); p.reject(new CdpClosedError()); }
    this.#pending.clear();
    this.emit('close');
  }
}

module.exports = { CdpConnection, CdpError, CdpClosedError, CdpTimeoutError };
