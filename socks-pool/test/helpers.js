'use strict';
/** Independent SOCKS5 server + HTTP target for tests (does not reuse src/ parsing). */
const net = require('node:net');
const http = require('node:http');

function listen(server) {
  return new Promise((resolve) => server.listen(0, '127.0.0.1', () => resolve(server.address().port)));
}

/**
 * Mock upstream proxy. Whatever hostname the client asks for, it connects to targetPort on
 * 127.0.0.1, and records what was asked, so tests can prove DNS is delegated to the proxy.
 */
async function startMockSocks({ username, password, targetPort }) {
  const seen = [];
  const sockets = new Set();
  const server = net.createServer((sock) => {
    sockets.add(sock);
    sock.on('close', () => sockets.delete(sock));
    sock.on('error', () => {}); // mock: peers resetting is normal here
    let buf = Buffer.alloc(0);
    let stage = 'greeting';
    let user;

    const step = () => {
      for (;;) {
        if (stage === 'greeting') {
          if (buf.length < 2 || buf.length < 2 + buf[1]) return;
          const methods = [...buf.subarray(2, 2 + buf[1])];
          buf = buf.subarray(2 + buf[1]);
          if (username !== undefined) {
            if (!methods.includes(2)) { sock.end(Buffer.from([5, 0xff])); return; }
            sock.write(Buffer.from([5, 2])); stage = 'auth';
          } else {
            sock.write(Buffer.from([5, 0])); stage = 'request';
          }
        } else if (stage === 'auth') {
          if (buf.length < 2 || buf.length < 3 + buf[1] + buf[2 + buf[1]]) return;
          const ulen = buf[1];
          const u = buf.subarray(2, 2 + ulen).toString();
          const plen = buf[2 + ulen];
          const p = buf.subarray(3 + ulen, 3 + ulen + plen).toString();
          buf = buf.subarray(3 + ulen + plen);
          if (u !== username || p !== password) { sock.end(Buffer.from([1, 1])); return; }
          user = u;
          sock.write(Buffer.from([1, 0])); stage = 'request';
        } else if (stage === 'request') {
          if (buf.length < 5) return;
          const atyp = buf[3];
          const addrLen = atyp === 1 ? 4 : atyp === 4 ? 16 : 1 + buf[4];
          if (buf.length < 4 + addrLen + 2) return;
          const host = atyp === 3 ? buf.subarray(5, 5 + buf[4]).toString()
            : atyp === 1 ? [...buf.subarray(4, 8)].join('.') : 'ipv6';
          const port = buf.readUInt16BE(4 + addrLen);
          const rest = buf.subarray(4 + addrLen + 2);
          buf = Buffer.alloc(0);
          stage = 'piping';
          sock.removeAllListeners('data');
          const target = net.connect(targetPort, '127.0.0.1');
          sockets.add(target);
          target.on('close', () => sockets.delete(target));
          target.on('error', () => sock.end(Buffer.from([5, 5, 0, 1, 0, 0, 0, 0, 0, 0])));
          target.once('connect', () => {
            seen.push({ host, port, user, atyp });
            sock.write(Buffer.from([5, 0, 0, 1, 0, 0, 0, 0, 0, 0]));
            if (rest.length) target.write(rest);
            sock.pipe(target); target.pipe(sock);
          });
          sock.on('close', () => target.destroy());
          return;
        } else return;
      }
    };
    sock.on('data', (d) => { buf = Buffer.concat([buf, d]); step(); });
  });
  const port = await listen(server);
  return {
    port, seen,
    spec: username === undefined ? `socks5://127.0.0.1:${port}` : `socks5://${username}:${password}@127.0.0.1:${port}`,
    close: () => { for (const s of sockets) s.destroy(); return new Promise((r) => server.close(r)); },
  };
}

/** HTTP target that answers every request with a fixed body and echoes the Host header. */
async function startTarget() {
  const hosts = [];
  const server = http.createServer((req, res) => {
    hosts.push(req.headers.host);
    res.writeHead(200, { 'content-type': 'text/html', 'cache-control': 'no-store' });
    res.end('<!doctype html><title>target-ok</title><body>hello</body>');
  });
  const port = await listen(server);
  return { port, hosts, close: () => { server.closeAllConnections(); return new Promise((r) => server.close(r)); } };
}

/** A port nothing listens on. */
async function deadPort() {
  const s = net.createServer();
  const port = await listen(s);
  await new Promise((r) => s.close(r));
  return port;
}

module.exports = { startMockSocks, startTarget, deadPort };
