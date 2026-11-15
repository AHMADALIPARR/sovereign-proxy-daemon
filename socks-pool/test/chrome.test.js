'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { ProxyPool, ChromeSession, NavigationError, PoolExhaustedError, EvaluationError, Socks5Error, withSession } = require('../src');
const { startMockSocks, startTarget, deadPort } = require('./helpers');

const SITE = (port) => `http://pool-test.invalid:${port}/`;

test('chrome: page loads through upstream with remote DNS; rotate switches exit', { timeout: 60000 }, async () => {
  const target = await startTarget();
  const A = await startMockSocks({ username: 'a', password: 'pa', targetPort: target.port });
  const B = await startMockSocks({ username: 'b', password: 'pb', targetPort: target.port });
  const pool = new ProxyPool([A.spec, B.spec]);
  const session = await ChromeSession.launch({ pool, proxyLoopback: true });
  const relayErrors = [];
  session.on('relay-error', (e) => relayErrors.push(e));
  try {
    const r1 = await session.goto(SITE(target.port));
    assert.equal(r1.status, 200);
    assert.equal(await session.evaluate('document.title'), 'target-ok');
    assert.equal(await session.evaluate('location.host'), `pool-test.invalid:${target.port}`);
    assert.ok(A.seen.some((s) => s.host === 'pool-test.invalid' && s.atyp === 3 && s.user === 'a'), 'A got hostname, authenticated');
    assert.equal(B.seen.length, 0);

    await session.evaluate("document.cookie = 'k=v'");
    const aBefore = A.seen.length;
    const to = await session.rotate();
    assert.equal(to.label, `127.0.0.1:${B.port}`);
    assert.equal(await session.evaluate("document.cookie === ''"), true, 'cookies cleared on rotate');

    await session.goto(SITE(target.port));
    assert.ok(B.seen.some((s) => s.host === 'pool-test.invalid' && s.user === 'b'), 'B used after rotate');
    assert.equal(A.seen.length, aBefore, 'no new connections via A after rotate');

    // The only alternative to B is A. Put A in cooldown: rotate must fail cleanly and keep B.
    const probe = pool.acquire({ exclude: [session.proxy.id] });
    probe.release();
    pool.reportFailure(probe.proxy, new Socks5Error('simulated', { kind: 'network' }));
    const exitBefore = session.proxy.id;
    await assert.rejects(session.rotate(), PoolExhaustedError);
    assert.equal(session.proxy.id, exitBefore, 'exit unchanged after failed rotate');
    assert.equal(pool.stats().find((s) => s.id === exitBefore).inflight, 1, 'lease on current exit intact');
    pool.reportSuccess(probe.proxy);
    assert.equal((await session.rotate()).id, probe.proxy.id, 'rotates back once A recovers');

    await assert.rejects(session.evaluate('throw new Error("boom")'), EvaluationError);
    assert.deepEqual(relayErrors, []);
  } finally { await session.close(); await A.close(); await B.close(); await target.close(); }
});

test('chrome: failover - dead proxy is penalised and navigation retries on a healthy one', { timeout: 60000 }, async () => {
  const target = await startTarget();
  const good = await startMockSocks({ targetPort: target.port });
  const dead = await deadPort();
  const pool = new ProxyPool([`127.0.0.1:${dead}`, good.spec], { strategy: 'round-robin' });
  try {
    const out = await withSession({ pool, proxyLoopback: true }, async (s) => {
      assert.equal(s.proxy.port, dead, 'starts on the dead proxy');
      const res = await s.navigateWithRotation(SITE(target.port), { attempts: 3 });
      return { res, exit: s.proxy.port };
    });
    assert.equal(out.res.status, 200);
    assert.equal(out.exit, good.port);
    const [d, g] = pool.stats();
    assert.ok(d.failures >= 1 && d.status !== 'healthy', `dead proxy penalised: ${JSON.stringify(d)}`);
    assert.equal(g.inflight, 0, 'leases released on close');
    assert.equal(d.inflight, 0);
  } finally { await good.close(); await target.close(); }
});

test('chrome: non-proxy failure (target refuses) is thrown, not retried/rotated', { timeout: 60000 }, async () => {
  const target = await startTarget();
  const up = await startMockSocks({ targetPort: await deadPort() });   // proxy fine, destination refuses
  const spare = await startMockSocks({ targetPort: target.port });
  const pool = new ProxyPool([up.spec, spare.spec]);
  try {
    await withSession({ pool, proxyLoopback: true }, async (s) => {
      await assert.rejects(s.navigateWithRotation(SITE(1234), { attempts: 3 }), NavigationError);
      assert.equal(s.proxy.port, up.port, 'did not rotate');
      assert.equal(pool.stats()[0].failures, 0, 'proxy not penalised for destination failure');
    });
  } finally { await up.close(); await spare.close(); await target.close(); }
});
