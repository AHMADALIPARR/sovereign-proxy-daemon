'use strict';
/**
 * Local, loopback-only SOCKS5 relay with a hot-swappable upstream.
 *
 * Why it exists: Chromium cannot authenticate to SOCKS5 proxies, and its proxy
 * settings are fixed at launch. Chromium points at this relay (no auth); the
 * relay authenticates to the real upstream and can switch upstream at any time
 * without restarting the browser. setUpstream() also tears down open tunnels so
 * keep-alive connections cannot keep exiting through the old IP.
 *
 * Events (never 'error', so an unhandled one cannot crash the process):
 *   'proxy-ok'      {upstream, host, port, latencyMs}
 *   'proxy-fail'    {upstream, host, port, error}   failure attributable to the proxy
 *   'target-fail'   {upstream, host, port, error}   destination refused/unreachable
 *   'tunnel-closed' {upstream, host, port, error?}
 *   'client-error'  {error}                         local client misbehaved or hit a bug
 *   'server-error'  {error}
 */
const net = require('node:net');
const { EventEmitter } = require('node:events');
const { ByteReader, Socks5Error, connectViaSocks5, readAddress } = require('./socks5');

const reply = (code) => Buffer.from([5, code, 0, 1, 0, 0, 0, 0, 0, 0]);
const MAX_ROTATION_RETRIES = 2;

function isClientHangup(err) {
  return (err instanceof Socks5Error && err.kind === 'closed')
    || err.code === 'ECONNRESET' || err.code === 'EPIPE';
}

class SocksRelay extends EventEmitter {
  #server = null;
  #upstream;
  #gen = 0;
  #sockets = new Set();

  constructor({ upstream, connectTimeoutMs = 10000 }) {
    super();
    this.#upstream = upstream;
    this.connectTimeoutMs = connectTimeoutMs;
  }

  get port() { return this.#server?.address()?.port; }
  get upstream() { return this.#upstream; }

  async listen() {
    this.#server = net.createServer((client) => {
      this.#track(client);
      this.#handle(client).catch((error) => {
        client.destroy();
        // A client hanging up before finishing the handshake is routine (browser preconnect
        // sockets, or setUpstream() dropping sockets). Everything else is reported.
        if (!isClientHangup(error)) this.emit('client-error', { error });
      });
    });
    await new Promise((resolve, reject) => {
      this.#server.once('error', reject);
      this.#server.listen(0, '127.0.0.1', () => {
        this.#server.off('error', reject);
        resolve();
      });
    });
    this.#server.on('error', (error) => this.emit('server-error', { error }));
  }

  /** Switch exit. Returns number of sockets dropped. */
  setUpstream(proxy) {
    this.#upstream = proxy;
    this.#gen++;
    const n = this.#sockets.size;
    for (const s of this.#sockets) s.destroy();
    return n;
  }

  async close() {
    if (!this.#server) return;
    for (const s of this.#sockets) s.destroy();
    await new Promise((resolve) => this.#server.close(resolve));
    this.#server = null;
  }

  #track(socket) {
    this.#sockets.add(socket);
    socket.once('close', () => this.#sockets.delete(socket));
  }

  async #handle(client) {
    client.setNoDelay(true);
    client.setTimeout(this.connectTimeoutMs + 5000, () => client.destroy());
    const reader = new ByteReader(client);

    const [ver, nMethods] = await reader.read(2);
    const methods = await reader.read(nMethods);
    if (ver !== 5 || !methods.includes(0)) { client.end(Buffer.from([5, 0xff])); return; }
    client.write(Buffer.from([5, 0]));

    const [rv, cmd, , atyp] = await reader.read(4);
    if (rv !== 5) { client.destroy(); return; }
    let host;
    try {
      host = await readAddress(reader, atyp);
    } catch (err) {
      if (!(err instanceof Socks5Error) || err.replyCode !== 8) throw err;
      client.end(reply(8));
      return;
    }
    const [hi, lo] = await reader.read(2);
    const port = (hi << 8) | lo;
    if (cmd !== 1) { client.end(reply(7)); return; } // only CONNECT

    let upstream; let upSock; let latencyMs;
    for (let attempt = 0; ; attempt++) {
      upstream = this.#upstream;
      const gen = this.#gen;
      try {
        ({ socket: upSock, latencyMs } = await connectViaSocks5({
          proxy: upstream, host, port, timeoutMs: this.connectTimeoutMs,
        }));
      } catch (error) {
        if (!(error instanceof Socks5Error)) throw error;
        if (gen !== this.#gen && attempt < MAX_ROTATION_RETRIES) continue; // rotated mid-connect: retry on new exit
        this.emit(error.blamesProxy ? 'proxy-fail' : 'target-fail', { upstream, host, port, error });
        client.end(reply(error.kind === 'reply' ? error.replyCode : 1));
        return;
      }
      if (gen === this.#gen) break;
      upSock.destroy(); // never let a connection finished on the old exit leak through
      if (attempt >= MAX_ROTATION_RETRIES) { client.end(reply(1)); return; }
    }

    this.emit('proxy-ok', { upstream, host, port, latencyMs });
    this.#track(upSock);
    client.setTimeout(0);
    const rest = reader.detach();
    client.write(reply(0));
    if (rest.length) upSock.write(rest);

    let closed = false;
    const teardown = (error) => {
      if (closed) return;
      closed = true;
      client.destroy();
      upSock.destroy();
      this.emit('tunnel-closed', { upstream, host, port, error });
    };
    client.on('error', teardown);
    upSock.on('error', teardown);
    client.on('close', () => teardown());
    upSock.on('close', () => teardown());
    client.pipe(upSock);
    upSock.pipe(client);
  }
}

module.exports = { SocksRelay };
