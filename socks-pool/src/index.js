'use strict';
const socks5 = require('./socks5');
const { SocksRelay } = require('./relay');
const pool = require('./pool');
const cdp = require('./cdp');
const browser = require('./browser');

/** Launch a session, run fn(session), always close it. */
async function withSession(options, fn) {
  const session = await browser.ChromeSession.launch(options);
  try {
    return await fn(session);
  } finally {
    await session.close();
  }
}

module.exports = { ...socks5, SocksRelay, ...pool, ...cdp, ...browser, withSession };
