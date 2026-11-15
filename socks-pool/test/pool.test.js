'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const { ProxyPool, PoolExhaustedError, parseProxy, parseProxyList, Socks5Error } = require('../src');

const specs = ['socks5://10.0.0.1:1080', '10.0.0.2:1080', '10.0.0.3:1080:u:p%'];
const mk = (opts = {}) => {
  const clock = { t: 1_000_000 };
  const pool = new ProxyPool(specs, { now: () => clock.t, ...opts });
  return { pool, clock };
};
const take = (pool, n, req) => Array.from({ length: n }, () => { const l = pool.acquire(req); l.release(); return l.proxy.host; });

test('parseProxy handles every supported format', () => {
  assert.deepEqual(parseProxy('socks5://u%40x:p%3Aw@h.example:9050'), { host: 'h.example', port: 9050, username: 'u@x', password: 'p:w' });
  assert.deepEqual(parseProxy('socks5h://1.2.3.4'), { host: '1.2.3.4', port: 1080, username: undefined, password: undefined });
  assert.deepEqual(parseProxy('1.2.3.4:1081'), { host: '1.2.3.4', port: 1081, username: undefined, password: undefined });
  assert.deepEqual(parseProxy('1.2.3.4:1081:bob:secret'), { host: '1.2.3.4', port: 1081, username: 'bob', password: 'secret' });
  assert.deepEqual(parseProxy('bob:se:cret@1.2.3.4:1081'), { host: '1.2.3.4', port: 1081, username: 'bob', password: 'se:cret' });
  assert.equal(parseProxy('socks5://[::1]:1080').host, '::1');
  assert.throws(() => parseProxy('http://1.2.3.4:80'), /unsupported scheme/);
  assert.throws(() => parseProxy('1.2.3.4:99999'), /bad port/);
  assert.throws(() => parseProxy('nonsense'), /expected host:port/);
});

test('parseProxyList skips comments/blank lines and reports line numbers', () => {
  assert.equal(parseProxyList('# c\n\n1.1.1.1:1\n2.2.2.2:2\n').length, 2);
  assert.throws(() => parseProxyList('1.1.1.1:1\nbad'), /line 2/);
});

test('round-robin cycles and skips cooling proxies', () => {
  const { pool } = mk();
  assert.deepEqual(take(pool, 4, {}), ['10.0.0.1', '10.0.0.2', '10.0.0.3', '10.0.0.1']);
  const l = pool.acquire(); l.release();                       // -> 10.0.0.2
  pool.reportFailure(pool.acquire().proxy, new Socks5Error('x', { kind: 'network' })); // fails 10.0.0.3
  assert.deepEqual(take(pool, 3, {}), ['10.0.0.1', '10.0.0.2', '10.0.0.1']);
});

test('failure cooldown backs off exponentially, then goes dead and recovers on retry window', () => {
  const { pool, clock } = mk({ cooldownBaseMs: 1000, maxFailures: 3, deadRetryMs: 60000 });
  const e = pool.acquire().proxy;
  const err = new Socks5Error('x', { kind: 'network' });
  pool.reportFailure(e, err); assert.equal(e.cooldownUntil - clock.t, 1000);
  pool.reportFailure(e, err); assert.equal(e.cooldownUntil - clock.t, 2000);
  assert.equal(pool.stats()[0].status, 'cooling');
  pool.reportFailure(e, err); assert.equal(e.cooldownUntil - clock.t, 60000);
  assert.equal(pool.stats()[0].status, 'dead');
  clock.t += 60001;
  assert.equal(pool.stats()[0].status, 'healthy');           // half-open: selectable again
  pool.reportSuccess(e, 10);
  assert.equal(e.consecutiveFailures, 0);
});

test('destination-side SOCKS replies are not held against the proxy', () => {
  const { pool } = mk();
  const e = pool.acquire().proxy;
  assert.equal(pool.reportFailure(e, new Socks5Error('r', { kind: 'reply', replyCode: 5 })), false);
  assert.equal(pool.reportFailure(e, new Socks5Error('r', { kind: 'reply', replyCode: 2 })), true);
  assert.equal(e.totalFailures, 1);
});

test('exhaustion throws PoolExhaustedError naming states, never credentials', () => {
  const { pool } = mk();
  const err = new Socks5Error('x', { kind: 'network' });
  for (let i = 0; i < 3; i++) pool.reportFailure(pool.acquire().proxy, err);
  assert.throws(() => pool.acquire(), (e) => e instanceof PoolExhaustedError && /cooling/.test(e.message) && !/:p%/.test(e.message));
});

test('exclude, maxPerProxy and lease release', () => {
  const { pool } = mk({ maxPerProxy: 1 });
  const a = pool.acquire(); const b = pool.acquire(); const c = pool.acquire();
  assert.deepEqual([a, b, c].map((l) => l.proxy.host), ['10.0.0.1', '10.0.0.2', '10.0.0.3']);
  assert.throws(() => pool.acquire(), PoolExhaustedError);
  a.release(); a.release();                                   // idempotent
  assert.equal(pool.acquire().proxy.host, '10.0.0.1');
  assert.throws(() => mk().pool.acquire({ exclude: ['1', '2', '3'] }), PoolExhaustedError);
});

test('sticky keys pin a caller until the proxy is unhealthy or excluded', () => {
  const { pool } = mk({ strategy: 'random', random: (() => { let i = 0; return () => [0.0, 0.9, 0.5, 0.9][i++ % 4]; })() });
  const first = pool.acquire({ key: 'k' }); first.release();
  for (let i = 0; i < 5; i++) { const l = pool.acquire({ key: 'k' }); l.release(); assert.equal(l.proxy.id, first.proxy.id); }
  const moved = pool.acquire({ key: 'k', exclude: [first.proxy.id] });
  assert.notEqual(moved.proxy.id, first.proxy.id);
  const l = pool.acquire({ key: 'k' }); assert.equal(l.proxy.id, moved.proxy.id);   // sticky follows the move
});

test('latency strategy explores unmeasured proxies then prefers the fastest', () => {
  const { pool } = mk({ strategy: 'latency' });
  const [a, b, c] = ['1', '2', '3'].map((n) => pool.acquire({ exclude: ['1', '2', '3'].filter((x) => x !== n) }).proxy);
  pool.reportSuccess(a, 300); pool.reportSuccess(b, 50); pool.reportSuccess(c, 120);
  assert.deepEqual(take(pool, 3, {}), ['10.0.0.2', '10.0.0.2', '10.0.0.2']);
});

test('least-used balances inflight then total uses', () => {
  const { pool } = mk({ strategy: 'least-used' });
  assert.deepEqual(take(pool, 6, {}).sort(), ['10.0.0.1', '10.0.0.1', '10.0.0.2', '10.0.0.2', '10.0.0.3', '10.0.0.3']);
});

test('construction validates strategy and non-empty list', () => {
  assert.throws(() => new ProxyPool(specs, { strategy: 'nope' }), RangeError);
  assert.throws(() => new ProxyPool([]), RangeError);
});
