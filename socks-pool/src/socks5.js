'use strict';
/**
 * Minimal SOCKS5 (RFC 1928) client with username/password auth (RFC 1929).
 *
 * Hostnames are always sent to the proxy as ATYP=DOMAIN, so DNS resolution
 * happens on the proxy side (no local DNS leak).
 */
const net = require('node:net');
const tls = require('node:tls');
const { once } = require('node:events');

const REPLY_TEXT = {
  1: 'general SOCKS server failure',
  2: 'connection not allowed by ruleset',
  3: 'network unreachable',
  4: 'host unreachable',
  5: 'connection refused',
  6: 'TTL expired',
  7: 'command not supported',
  8: 'address type not supported',
};
// Reply codes that describe the destination rather than the proxy itself.
const TARGET_SIDE_REPLIES = new Set([3, 4, 5, 6]);
const NETWORK_ERROR_CODES = new Set([
  'ECONNREFUSED', 'ECONNRESET', 'ETIMEDOUT', 'EHOSTUNREACH', 'ENETUNREACH', 'ENOTFOUND', 'EPIPE',
]);

class Socks5Error extends Error {
  /**
   * @param {string} message
   * @param {{kind: 'network'|'auth'|'protocol'|'reply'|'closed', replyCode?: number, cause?: Error}} info
   */
  constructor(message, { kind, replyCode, cause }) {
    super(message, cause ? { cause } : undefined);
    this.name = 'Socks5Error';
    this.kind = kind;
    this.replyCode = replyCode;
  }
  /** True when the failure says something about the proxy, not the destination. */
  get blamesProxy() {
    return this.kind === 'reply' ? !TARGET_SIDE_REPLIES.has(this.replyCode) : true;
  }
}

/** Promise-style exact-length reader over a socket, with leftover hand-off. */
class ByteReader {
  #socket; #buf = Buffer.alloc(0); #waiter = null; #ended = null;
  #onData = (chunk) => { this.#buf = Buffer.concat([this.#buf, chunk]); this.#wake(); };
  #onClose = () => {
    this.#ended ??= new Socks5Error('connection closed during handshake', { kind: 'closed' });
    this.#wake();
  };
  #onError = (err) => { this.#ended = err; this.#wake(); };

  constructor(socket) {
    this.#socket = socket;
    socket.on('data', this.#onData);
    socket.on('close', this.#onClose);
    socket.on('error', this.#onError);
  }
  read(n) {
    return new Promise((resolve, reject) => { this.#waiter = { n, resolve, reject }; this.#wake(); });
  }
  #wake() {
    const w = this.#waiter;
    if (!w) return;
    if (this.#buf.length >= w.n) {
      this.#waiter = null;
      const out = this.#buf.subarray(0, w.n);
      this.#buf = this.#buf.subarray(w.n);
      w.resolve(out);
    } else if (this.#ended) {
      this.#waiter = null;
      w.reject(this.#ended);
    }
  }
  /** Stop reading; returns bytes received beyond what was consumed. */
  detach() {
    this.#socket.off('data', this.#onData);
    this.#socket.off('close', this.#onClose);
    this.#socket.off('error', this.#onError);
    const rest = this.#buf;
    this.#buf = Buffer.alloc(0);
    return rest;
  }
}

function ipv6ToBuffer(ip) {
  const bare = ip.split('%')[0];
  if (bare.includes('.')) throw new Socks5Error(`IPv4-embedded IPv6 not supported: ${ip}`, { kind: 'protocol' });
  const [head, tail] = bare.split('::');
  const h = head ? head.split(':') : [];
  const t = tail === undefined ? [] : tail ? tail.split(':') : [];
  const fill = tail === undefined ? 0 : 8 - h.length - t.length;
  const groups = [...h, ...Array(fill).fill('0'), ...t];
  if (groups.length !== 8) throw new Socks5Error(`bad IPv6 address: ${ip}`, { kind: 'protocol' });
  const buf = Buffer.alloc(16);
  groups.forEach((g, i) => buf.writeUInt16BE(parseInt(g, 16), i * 2));
  return buf;
}

function encodeAddress(host) {
  const family = net.isIP(host);
  if (family === 4) return Buffer.concat([Buffer.from([1]), Buffer.from(host.split('.').map(Number))]);
  if (family === 6) return Buffer.concat([Buffer.from([4]), ipv6ToBuffer(host)]);
  const name = Buffer.from(host, 'utf8');
  if (name.length === 0 || name.length > 255) {
    throw new Socks5Error(`hostname length ${name.length} not encodable`, { kind: 'protocol' });
  }
  return Buffer.concat([Buffer.from([3, name.length]), name]);
}

async function readAddress(reader, atyp) {
  if (atyp === 1) return Array.from(await reader.read(4)).join('.');
  if (atyp === 3) {
    const [len] = await reader.read(1);
    return (await reader.read(len)).toString('utf8');
  }
  if (atyp === 4) {
    const b = await reader.read(16);
    const groups = [];
    for (let i = 0; i < 16; i += 2) groups.push(b.readUInt16BE(i).toString(16));
    return groups.join(':');
  }
  throw new Socks5Error(`unsupported address type ${atyp}`, { kind: 'protocol', replyCode: 8 });
}

function classify(err, proxy) {
  if (err instanceof Socks5Error) return err;
  if (NETWORK_ERROR_CODES.has(err.code)) {
    return new Socks5Error(`upstream ${proxy.host}:${proxy.port}: ${err.code}`, { kind: 'network', cause: err });
  }
  return err; // anything else is a bug, not a network condition: let it propagate unchanged
}

/**
 * Open a TCP tunnel to host:port through a SOCKS5 proxy.
 * Resolves with a paused net.Socket that is already past the handshake.
 *
 * @param {{proxy: {host: string, port: number, username?: string, password?: string},
 *          host: string, port: number, timeoutMs?: number}} opts
 */
async function connectViaSocks5({ proxy, host, port, timeoutMs = 10000 }) {
  const t0 = performance.now();
  const socket = net.connect({ host: proxy.host, port: proxy.port });
  socket.setNoDelay(true);
  socket.setTimeout(timeoutMs, () => {
    socket.destroy(Object.assign(new Error(`timed out after ${timeoutMs}ms`), { code: 'ETIMEDOUT' }));
  });
  const reader = new ByteReader(socket);
  try {
    await once(socket, 'connect');

    const hasAuth = Boolean(proxy.username);
    socket.write(Buffer.from(hasAuth ? [5, 2, 0, 2] : [5, 1, 0]));
    const [ver, method] = await reader.read(2);
    if (ver !== 5) throw new Socks5Error(`upstream is not SOCKS5 (version byte ${ver})`, { kind: 'protocol' });
    if (method === 0xff) throw new Socks5Error('upstream rejected all offered auth methods', { kind: 'auth' });
    if (method === 2) {
      if (!hasAuth) throw new Socks5Error('upstream requires credentials but none are configured', { kind: 'auth' });
      const u = Buffer.from(proxy.username);
      const p = Buffer.from(proxy.password ?? '');
      if (u.length > 255 || p.length > 255) throw new Socks5Error('credentials longer than 255 bytes', { kind: 'auth' });
      socket.write(Buffer.concat([Buffer.from([1, u.length]), u, Buffer.from([p.length]), p]));
      const [, status] = await reader.read(2);
      if (status !== 0) throw new Socks5Error('upstream rejected credentials', { kind: 'auth' });
    } else if (method !== 0) {
      throw new Socks5Error(`upstream selected unsupported auth method ${method}`, { kind: 'protocol' });
    }

    const portBuf = Buffer.alloc(2);
    portBuf.writeUInt16BE(port);
    socket.write(Buffer.concat([Buffer.from([5, 1, 0]), encodeAddress(host), portBuf]));
    const [rv, rep, , atyp] = await reader.read(4);
    if (rv !== 5) throw new Socks5Error(`bad reply version ${rv}`, { kind: 'protocol' });
    if (rep !== 0) {
      throw new Socks5Error(
        `upstream refused CONNECT ${host}:${port}: ${REPLY_TEXT[rep] ?? `reply ${rep}`}`,
        { kind: 'reply', replyCode: rep },
      );
    }
    await readAddress(reader, atyp); // bound address (ignored)
    await reader.read(2);            // bound port (ignored)

    const rest = reader.detach();
    socket.setTimeout(0);
    if (rest.length) socket.unshift(rest);
    socket.pause();
    return { socket, latencyMs: performance.now() - t0 };
  } catch (err) {
    socket.destroy();
    throw classify(err, proxy);
  }
}

/** Plain HTTP/1.0 GET through a proxy; used for exit-IP probes. Supports http and https. */
async function httpGetViaSocks5({ proxy, url, timeoutMs = 15000, maxBytes = 1 << 20 }) {
  const u = new URL(url);
  const port = Number(u.port) || (u.protocol === 'https:' ? 443 : 80);
  const { socket } = await connectViaSocks5({ proxy, host: u.hostname, port, timeoutMs });
  const stream = u.protocol === 'https:' ? tls.connect({ socket, servername: u.hostname }) : socket;
  stream.setTimeout(timeoutMs, () => {
    stream.destroy(Object.assign(new Error(`timed out after ${timeoutMs}ms`), { code: 'ETIMEDOUT' }));
  });
  stream.write(`GET ${u.pathname}${u.search} HTTP/1.0\r\nHost: ${u.host}\r\nUser-Agent: socks-pool\r\nConnection: close\r\n\r\n`);
  const chunks = [];
  let size = 0;
  for await (const chunk of stream) {
    size += chunk.length;
    if (size > maxBytes) { stream.destroy(); throw new Error(`response exceeded ${maxBytes} bytes`); }
    chunks.push(chunk);
  }
  const raw = Buffer.concat(chunks);
  const sep = raw.indexOf('\r\n\r\n');
  if (sep < 0) throw new Error('malformed HTTP response (no header terminator)');
  const m = /^HTTP\/\d\.\d (\d{3})/.exec(raw.subarray(0, sep).toString('latin1'));
  if (!m) throw new Error('malformed HTTP status line');
  return { status: Number(m[1]), body: raw.subarray(sep + 4).toString('utf8') };
}

module.exports = {
  Socks5Error, ByteReader, connectViaSocks5, httpGetViaSocks5, encodeAddress, readAddress, REPLY_TEXT,
};
