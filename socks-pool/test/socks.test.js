'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const net = require('node:net');
const { once } = require('node:events');
const { connectViaSocks5, httpGetViaSocks5, Socks5Error, SocksRelay, ProxyPool } = require('../src');
const { startMockSocks, startTarget, deadPort } = require('./helpers');

async function readAll(socket) { const c = []; for await (const d of socket) c.push(d); return Buffer.concat(c).toString(); }

test('client: user/pass auth, hostname delegated to proxy, data flows', async () => {
  const target = await startTarget();
  const up = await startMockSocks({ username: 'bob', password: 's3cret', targetPort: target.port });
  try {
    const { socket } = await connectViaSocks5({ proxy: { host: '127.0.0.1', port: up.port, username: 'bob', password: 's3cret' }, host: 'never-resolves.invalid', port: 8080 });
    socket.write('GET / HTTP/1.0\r\nHost: x\r\n\r\n');
    assert.match(await readAll(socket), /target-ok/);
    assert.deepEqual(up.seen.map(({ host, port, user, atyp }) => ({ host, port, user, atyp })),
      [{ host: 'never-resolves.invalid', port: 8080, user: 'bob', atyp: 3 }]);
  } finally { await up.close(); await target.close(); }
});

test('client: wrong credentials -> Socks5Error(kind auth) that blames the proxy', async () => {
  const up = await startMockSocks({ username: 'bob', password: 'right', targetPort: 1 });
  try {
    await assert.rejects(
      connectViaSocks5({ proxy: { host: '127.0.0.1', port: up.port, username: 'bob', password: 'wrong' }, host: 'a.invalid', port: 80 }),
      (e) => e instanceof Socks5Error && e.kind === 'auth' && e.blamesProxy);
    await assert.rejects(
      connectViaSocks5({ proxy: { host: '127.0.0.1', port: up.port }, host: 'a.invalid', port: 80 }),
      (e) => e instanceof Socks5Error && e.kind === 'auth');
  } finally { await up.close(); }
});

test('client: connection refused is classified as network; target-side reply does not blame proxy', async () => {
  const dead = await deadPort();
  await assert.rejects(connectViaSocks5({ proxy: { host: '127.0.0.1', port: dead }, host: 'a.invalid', port: 80 }),
    (e) => e instanceof Socks5Error && e.kind === 'network' && e.blamesProxy);
  const up = await startMockSocks({ targetPort: await deadPort() });   // proxy up, destination refuses -> reply 5
  try {
    await assert.rejects(connectViaSocks5({ proxy: { host: '127.0.0.1', port: up.port }, host: 'a.invalid', port: 80 }),
      (e) => e instanceof Socks5Error && e.kind === 'reply' && e.replyCode === 5 && !e.blamesProxy);
  } finally { await up.close(); }
});

test('client: handshake timeout against a silent server', async () => {
  const silent = net.createServer(() => {});
  silent.listen(0, '127.0.0.1'); await once(silent, 'listening');
  try {
    await assert.rejects(connectViaSocks5({ proxy: { host: '127.0.0.1', port: silent.address().port }, host: 'a.invalid', port: 80, timeoutMs: 200 }),
      (e) => e instanceof Socks5Error && e.kind === 'network' && /ETIMEDOUT/.test(e.message));
  } finally { silent.close(); }
});

test('httpGetViaSocks5 fetches through the proxy', async () => {
  const target = await startTarget();
  const up = await startMockSocks({ targetPort: target.port });
  try {
    const r = await httpGetViaSocks5({ proxy: { host: '127.0.0.1', port: up.port }, url: 'http://echo.invalid:9999/ip' });
    assert.equal(r.status, 200); assert.match(r.body, /target-ok/);
    assert.equal(target.hosts[0], 'echo.invalid:9999');
  } finally { await up.close(); await target.close(); }
});

test('pool.check: reports success, and failure counted against a dead proxy', async () => {
  const target = await startTarget();
  const up = await startMockSocks({ targetPort: target.port });
  const dead = await deadPort();
  const pool = new ProxyPool([up.spec, `127.0.0.1:${dead}`], { checkTarget: { host: 'a.invalid', port: 80 } });
  try {
    const res = await pool.checkAll();
    assert.deepEqual(res.map((r) => [r.id, r.ok]).sort(), [['1', true], ['2', false]]);
    assert.equal(pool.stats()[1].consecutiveFailures, 1);
    assert.ok(pool.stats()[0].latencyMs >= 0);
  } finally { await up.close(); await target.close(); }
});

/** Tiny no-auth SOCKS5 client used to talk to the relay. */
async function viaRelay(relayPort, host, port, request) {
  const s = net.connect(relayPort, '127.0.0.1');
  await once(s, 'connect');
  s.write(Buffer.from([5, 1, 0]));
  const hdr = Buffer.alloc(0);
  const next = (n) => new Promise((res) => { const h = () => { const b = s.read(n); if (b) { s.off('readable', h); res(b); } }; s.on('readable', h); h(); });
  assert.deepEqual([...await next(2)], [5, 0]);
  const name = Buffer.from(host);
  s.write(Buffer.concat([Buffer.from([5, 1, 0, 3, name.length]), name, Buffer.from([port >> 8, port & 255])]));
  const rep = await next(10);
  void hdr;
  return { s, rep: rep[1], send: () => s.write(request) };
}

test('relay: authenticates upstream for a no-auth client, rotates, and drops old tunnels', async () => {
  const target = await startTarget();
  const A = await startMockSocks({ username: 'a', password: 'pa', targetPort: target.port });
  const B = await startMockSocks({ username: 'b', password: 'pb', targetPort: target.port });
  const pool = new ProxyPool([A.spec, B.spec]);
  const relay = new SocksRelay({ upstream: pool.acquire().proxy });
  const events = [];
  for (const ev of ['proxy-ok', 'proxy-fail', 'client-error']) relay.on(ev, (e) => events.push([ev, e]));
  await relay.listen();
  try {
    const c1 = await viaRelay(relay.port, 'site.invalid', 80, 'GET / HTTP/1.0\r\n\r\n');
    assert.equal(c1.rep, 0);
    c1.send();
    assert.match(await readAll(c1.s), /target-ok/);
    assert.equal(A.seen.length, 1); assert.equal(A.seen[0].user, 'a'); assert.equal(B.seen.length, 0);

    // keep a tunnel open, rotate, and verify it is torn down
    const c2 = await viaRelay(relay.port, 'site.invalid', 80, '');
    const closed = once(c2.s, 'close');
    assert.ok(relay.setUpstream(pool.acquire().proxy) >= 1);
    await closed;

    const c3 = await viaRelay(relay.port, 'site.invalid', 80, 'GET / HTTP/1.0\r\n\r\n');
    c3.send(); await readAll(c3.s);
    assert.equal(B.seen.length, 1); assert.equal(B.seen[0].user, 'b');
    assert.equal(A.seen.length, 2); // c1 + c2 only; nothing after rotation
    assert.deepEqual(events.filter(([k]) => k !== 'proxy-ok'), []);
  } finally { await relay.close(); await A.close(); await B.close(); await target.close(); }
});

test('relay: unreachable upstream -> proxy-fail event and SOCKS failure reply', async () => {
  const dead = await deadPort();
  const relay = new SocksRelay({ upstream: { host: '127.0.0.1', port: dead } });
  const fails = [];
  relay.on('proxy-fail', (e) => fails.push(e));
  await relay.listen();
  try {
    const c = await viaRelay(relay.port, 'site.invalid', 80, '');
    assert.equal(c.rep, 1);
    assert.equal(fails.length, 1);
    assert.equal(fails[0].error.kind, 'network');
    c.s.destroy();
  } finally { await relay.close(); }
});
