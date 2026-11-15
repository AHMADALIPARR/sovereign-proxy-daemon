#!/usr/bin/env node
'use strict';
const { parseArgs } = require('node:util');
const { ProxyPool, ChromeSession, parseProxy } = require('../src');

const USAGE = `usage: socks-pool [options] URL [URL...]

  --proxies FILE         proxy list, one per line (socks5://u:p@host:port | host:port | host:port:user:pass)
  --proxy SPEC           add one proxy (repeatable)
  --strategy NAME        round-robin | random | least-used | latency   (default round-robin)
  --rotate-every N       navigations per exit before rotating           (default 1)
  --attempts N           retries on proxy failure per URL               (default 3)
  --eval EXPR            JS evaluated in the page after load, result printed
  --check                health-check every proxy first and print the table
  --check-target H:P     destination used by health checks              (default example.com:443)
  --chrome PATH          Chrome/Chromium binary (or CHROME_PATH)
  --no-headless          show the browser window
  --no-sandbox           pass --no-sandbox to Chrome (automatic when root)

Output: one JSON object per URL on stdout. Credentials are never printed.`;

async function main() {
  const { values: v, positionals: urls } = parseArgs({
    allowPositionals: true,
    options: {
      proxies: { type: 'string' }, proxy: { type: 'string', multiple: true },
      strategy: { type: 'string', default: 'round-robin' },
      'rotate-every': { type: 'string', default: '1' }, attempts: { type: 'string', default: '3' },
      eval: { type: 'string' }, check: { type: 'boolean', default: false },
      'check-target': { type: 'string' }, chrome: { type: 'string' },
      'no-headless': { type: 'boolean', default: false }, 'no-sandbox': { type: 'boolean', default: false },
      help: { type: 'boolean', short: 'h', default: false },
    },
  });
  if (v.help || (urls.length === 0 && !v.check)) { console.log(USAGE); return; }

  const poolOpts = { strategy: v.strategy };
  if (v['check-target']) {
    const [host, port] = v['check-target'].split(':');
    poolOpts.checkTarget = { host, port: Number(port) };
  }
  const pool = v.proxies
    ? ProxyPool.fromFile(v.proxies, poolOpts)
    : new ProxyPool((v.proxy ?? []).map((s) => parseProxy(s)), poolOpts);

  if (v.check) {
    console.error(JSON.stringify(await pool.checkAll()));
    console.table(pool.stats());
    if (urls.length === 0) return;
  }

  const rotateEvery = Number(v['rotate-every']);
  const attempts = Number(v.attempts);
  const session = await ChromeSession.launch({
    pool, chromePath: v.chrome, headless: !v['no-headless'], sandbox: v['no-sandbox'] ? false : undefined,
  });
  session.on('rotate', (e) => console.error(`rotate ${e.from} -> ${e.to}`));
  session.on('relay-error', (e) => console.error(`relay error: ${e.message}`));
  try {
    let n = 0;
    for (const url of urls) {
      if (n > 0 && n % rotateEvery === 0) await session.rotate();
      n++;
      const { status } = await session.navigateWithRotation(url, { attempts });
      const out = { url, status, exit: session.proxy.label };
      if (v.eval) out.result = await session.evaluate(v.eval);
      console.log(JSON.stringify(out));
    }
  } finally {
    await session.close();
  }
}

main().catch((err) => { console.error(`${err.name}: ${err.message}`); process.exitCode = 1; });
