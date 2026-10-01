// SPDX-License-Identifier: Apache-2.0
// tools/smoke.mjs -- frontend smoke test (a dev tool; not part of the embed)
//
// Two scenarios, both black-box: driven only through DOM events, importing no
// internal symbols.
//   A  ?demo=1   a simulated event stream -> the render chain
//   B  no demo   real wiring: EventSource frames plus the fetch failure path (cas_failed)
//
// Each scenario copies web/app into its own temp directory before importing, so
// the ESM module cache cannot leak between them.

import { readFile, cp, rm } from 'node:fs/promises';
import { join, dirname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { JSDOM } from 'jsdom';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const APP = join(ROOT, 'web', 'app');
const HTML = await readFile(join(ROOT, 'web', 'index.html'), 'utf8');

let passed = 0, failed = 0;
const ok = (cond, label) => {
  if (cond) { passed++; console.log(`   \u2713 ${label}`); }
  else { failed++; console.log(`   \u2717 ${label}`); }
};
const flush = () => new Promise((r) => setImmediate(r));
const headline = (s) => console.log(`\n${s}`);

// ── A deterministic clock (taking over setTimeout/setInterval so the mock
//    timeline can be fast-forwarded) ─────────────────────────────────

let seq = 0, now = 0;
const timers = new Map();
globalThis.setTimeout = (fn, ms = 0, ...args) => { const id = ++seq; timers.set(id, { fn, at: now + ms, args, every: 0 }); return id; };
globalThis.setInterval = (fn, ms = 0, ...args) => { const id = ++seq; timers.set(id, { fn, at: now + ms, args, every: ms || 1 }); return id; };
globalThis.clearTimeout = (id) => timers.delete(id);
globalThis.clearInterval = (id) => timers.delete(id);

function advance(ms) {
  const end = now + ms;
  for (;;) {
    let pick = null;
    for (const [id, t] of timers) if (t.at <= end && (!pick || t.at < pick[1].at)) pick = [id, t];
    if (!pick) break;
    const [id, t] = pick;
    now = t.at;
    if (t.every) t.at = now + t.every; else timers.delete(id);
    t.fn(...t.args);
  }
  now = end;
}

// ── Isolated loading ─────────────────────────────────────────────

async function boot(tag, search) {
  timers.clear(); seq = 0; now = 0;

  const dom = new JSDOM(HTML, {
    url: `http://127.0.0.1:8099/${search}`,
    pretendToBeVisual: true,
  });

  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.location = dom.window.location;
  globalThis.requestAnimationFrame = dom.window.requestAnimationFrame.bind(dom.window);
  dom.window.scrollTo = () => { };                   // jsdom does not implement this
  dom.window.uiCalls = 0;
  globalThis.ui = () => { dom.window.uiCalls++; };   // Beer CSS's JS: a stub

  const dir = `/tmp/immulog-smoke-${tag}`;
  await rm(dir, { recursive: true, force: true });
  await cp(APP, dir, { recursive: true });
  await import(pathToFileURL(join(dir, 'main.js')).href);

  return dom;
}

// ── Scenario A: a simulated event stream -> rendering ───────────────

headline('Scenario A - ?demo=1 simulated timeline -> rendering');
{
  const dom = await boot('a', '?demo=1');
  const $ = (s) => dom.window.document.querySelectorAll(s);
  const one = (s) => dom.window.document.querySelector(s);

  ok($('.msg').length === 0, 'nothing rendered at first');

  advance(300); await flush();
  ok($('.msg').length === 1, 'the genesis message appears');
  ok(one('#room-id').textContent !== '--', `room identity = first 6 hex of the genesis OID (${one('#room-id').textContent})`);

  advance(600); await flush();
  ok($('.msg').length === 2, 'the second message is appended');

  advance(600); await flush();
  const bob = $('.msg')[2];
  ok(bob && bob.dataset.state === 'verified', 'bob\'s message is verified');
  ok(bob && bob.querySelector('.shape').className.includes('gem'), 'verified -> shape gem');

  advance(700); await flush();
  ok($('.msg').length === 4, 'the fourth message is appended');

  // Retraction
  const alice = $('.msg')[1];
  ok(alice.dataset.state === 'verified', 'before retraction: alice\'s message is verified');
  advance(1000); await flush();
  ok(alice.dataset.state === 'retracted', 'after retraction: the original entry is marked retracted, not deleted');
  ok(alice.querySelector('.shape').className.includes('slanted'), 'retraction -> the shape switches to slanted');
  ok(alice.querySelector('.strike-note').textContent.includes('retracted'), 'the strike-through note appears');
  ok($('.msg').length === 4, 'the message count is unchanged (retraction appends, it does not delete)');

  // Tampering alarm
  advance(1400); await flush();
  ok($('.alarm').length === 1, 'the alarm is sealed into the timeline');
  ok(one('.alarm .shape').className.includes('burst'), 'alarm -> shape burst');
  ok(one('#alarm-log .kv') !== null, 'the alarm also lands in the integrity tab log');
  ok(one('#alarm-log .placeholder') === null, 'the "none" placeholder is cleared from the alarm log');

  // Gossip: the peer *view* comparison, kept apart from the git-sync picture
  const peerRows = [...one('#peers').children];
  ok(peerRows.length === 6, `git remotes and gossip peers share the panel (${peerRows.length} rows)`);
  ok(peerRows.filter((r) => r.textContent.includes('· gossip')).length === 3, 'gossip peers are tagged so the two pictures stay distinguishable');
  ok(peerRows.filter((r) => r.textContent.includes('differs')).length === 2, 'disagreement is marked on both a remote and a peer');
  ok(one('#snapshot').textContent !== '--', `the snapshot event carries the digest peers are compared against (${one('#snapshot').textContent})`);

  // A feed peers can see and this node cannot is discovery, not tampering
  const missing = one('#missing-feeds .kv');
  ok(missing !== null, 'a feed only peers know about is listed');
  ok(missing.textContent.includes('2 peers'), 'the peer count is the signal -- two views agreeing beats one claim');
  ok($('.alarm').length === 1, 'discovery is never raised as an alarm');

  // Heartbeat
  advance(9000); await flush();
  ok($('.msg').length === 6, 'a heartbeat message arrives (the timeline is live)');

  // View switching
  one('#views button[data-view="integrity"]').click();
  ok(one('.view[data-view="integrity"]').classList.contains('active'), 'switching to the integrity tab');
  ok(!one('.view[data-view="chat"]').classList.contains('active'), 'the messages tab is deactivated');
  ok(one('#anchor-count').textContent !== '0', `the witness anchor count updated (${one('#anchor-count').textContent})`);

  // Theme
  const before = dom.window.document.body.classList.contains('dark');
  one('#theme').click();
  ok(dom.window.document.body.classList.contains('dark') !== before, 'light and dark themes switch');
  ok(dom.window.uiCalls > 0, 'the dynamic theme colour was handed to Beer CSS');

  // Optimistic delivery
  const dupBefore = $('.msg').length;
  one('#input').value = 'smoke test';
  one('#send').click();
  await flush();
  const mine = [...$('.msg')].at(-1);
  ok(mine.querySelector('.text').textContent.includes('smoke test'), 'optimistic delivery: the message renders first');
  ok($('.msg').length === dupBefore + 1, 'exactly one more message');
  const pendingShape = mine.querySelector('.shape').className;
  ok(pendingShape.includes('loading-indicator'), 'not landed -> shape loading-indicator');
  ok(mine.dataset.state === 'pending', 'not landed -> state pending');
  ok(mine.dataset.oid.startsWith('local-'), 'not landed -> still the temporary OID');

  advance(500); await flush();
  ok(mine.dataset.state === 'verified', 'lands and turns verified in place');
  ok(mine.dataset.oid.length === 40, 'the temporary OID became a real 40-char hash');
  ok(mine.querySelector('.shape').className.includes('gem'), 'shape becomes gem after landing (in place; the bubble does not jump)');
  ok(one('#input').value === '', 'the input is cleared');
}

// ── Scenario B: real wiring (SSE frames plus a CAS failure) ─────────

headline('Scenario B - no demo: EventSource frames + the fetch failure path');
{
  class FakeES {
    constructor(url) { this.url = url; this.listeners = {}; FakeES.last = this; }
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
    close() { this.closed = true; }
    emit(type, payload, id) {
      for (const fn of this.listeners[type] || []) fn({ type, data: JSON.stringify(payload), lastEventId: id });
    }
  }
  const fetchCalls = [];
  globalThis.EventSource = FakeES;
  globalThis.fetch = async (url, opts) => {
    fetchCalls.push({ url, body: JSON.parse(opts.body) });
    return { ok: false, status: 409, json: async () => ({ error: 'cas_failed', expected: 'aaa', actual: 'bbb' }) };
  };

  const dom = await boot('b', '');
  const $ = (s) => dom.window.document.querySelectorAll(s);
  const one = (s) => dom.window.document.querySelector(s);
  const OID = 'a'.repeat(40);
  const FEED = 'refs/feeds/alice';
  const OTHER_FEED = 'refs/feeds/mallory';

  ok(FakeES.last !== undefined, 'an SSE connection was opened');
  ok(FakeES.last.url === '/api/stream', 'connected to /api/stream');
  ok($('.msg').length === 0, 'nothing rendered at first (no faked first screen)');

  FakeES.last.emit('msg', { oid: OID, seq: 1, author: 'alice', feed: FEED, body: 'from the real stream' }, OID);
  await flush();
  ok($('.msg').length === 1, 'an SSE frame renders as a message');
  ok(one('.msg .text').textContent.includes('from the real stream'), 'the body is correct');

  FakeES.last.emit('msg', { oid: OID, seq: 1, author: 'alice', feed: FEED, body: 'from the real stream' }, OID);
  await flush();
  ok($('.msg').length === 1, 'the same OID dedupes and is not rendered twice');

  // A cross-feed retraction must be ignored: retraction is the author's right,
  // not something anyone can do to anyone
  FakeES.last.emit('retract',
    { oid: 'e'.repeat(40), feed: OTHER_FEED, retracts: OID, reason: 'unauthorised retraction' }, 'e'.repeat(40));
  await flush();
  ok(one('.msg').dataset.state === 'verified', 'a retraction from another feed is ignored (retraction applies within one feed)');

  FakeES.last.emit('retract',
    { oid: 'b'.repeat(40), feed: FEED, retracts: OID, reason: 'got it wrong' }, 'b'.repeat(40));
  await flush();
  ok(one('.msg').dataset.state === 'retracted', 'a retraction frame from the same feed takes effect');
  ok(one('.msg .strike-note').textContent.includes('got it wrong'), 'the retraction reason is shown');

  FakeES.last.emit('alarm',
    { oid: 'c'.repeat(40), title: 'History rewrite detected', local: '111111', remote: '222222' }, 'c'.repeat(40));
  await flush();
  ok($('.alarm').length === 1, 'an SSE alarm frame takes effect');

  FakeES.last.emit('hello', {
    head: OID, snapshot: 'abcdef1234567890', anchoredAt: '2026-10-01 12:00',
    identity: { signed: true, key: 'SHA256:abcdefghijklmnop' },
    peers: [{ name: 'hub', url: 'git@node-b', ok: true }],
  }, '');
  await flush();
  ok(one('#anchored-at').textContent.includes('12:00'), 'the hello frame updates the anchoring time');
  ok(one('#snapshot').textContent.startsWith('abcdef'), 'the hello frame updates the snapshot digest');
  ok(one('#identity-chip').textContent === 'signed', 'the hello frame updates the signing state');
  ok(one('#identity-line').textContent.includes('SHA256'), 'the hello frame updates the key fingerprint');
  ok(one('#peers .kv') !== null, 'the hello frame renders the peer list');

  // The view comparison arrives on the same frame but is a separate picture
  FakeES.last.emit('hello', {
    head: OID,
    gossip: [{ name: 'hub', url: 'http://node-b:8082', ok: false, note: 'knows feeds this node does not' }],
    missingFeeds: [{ feed: FEED, peers: 2 }],
  }, '');
  await flush();
  ok([...one('#peers').children].some((r) => r.textContent.includes('gossip')), 'the hello frame renders the gossip view comparison');
  ok(one('#missing-feeds .kv') !== null, 'the hello frame renders feeds peers hold and this node does not');

  FakeES.last.emit('snapshot', { digest: 'f'.repeat(40), feeds: 7 }, 'f'.repeat(40));
  await flush();
  ok(one('#snapshot').textContent === 'ffffff', 'a live snapshot frame moves the digest peers are compared against');

  // When unsigned it must say plainly that impersonation is possible
  FakeES.last.emit('hello', { head: OID, identity: { signed: false } }, '');
  await flush();
  ok(one('#identity-chip').textContent === 'unsigned', 'unsigned is labelled honestly');
  ok(one('#identity-line').textContent.includes('impersonated'), 'unsigned carries a risk note');

  // Sending: the server returns cas_failed
  one('#input').value = 'this one will be rejected';
  one('#send').click();
  await flush(); await flush();

  ok(fetchCalls.length === 1, 'the write entry issues exactly one request');
  ok(fetchCalls[0].url === '/api/commit', 'POSTs to /api/commit');
  ok(fetchCalls[0].body.kind === 'msg', 'kind distinguishes the event type');
  ok(one('.msg[data-state="unverified"]') !== null, 'cas_failed -> the message is marked unverified');
  ok(one('#toast').classList.contains('active'), 'cas_failed was not swallowed; the user was told');
  ok(one('#toast-text').textContent.includes('cas_failed'), `the notice carries the error code (${one('#toast-text').textContent})`);
}

// ── Stylesheet integrity: CSS has no // comments, and one stray line
//    silently eats every rule after it ───────────────────────────────
{
  const { readFile } = await import('node:fs/promises');
  const { JSDOM } = await import('jsdom');
  const path = new URL('../web/style.css', import.meta.url);
  const css = await readFile(path, 'utf8');

  const dom = new JSDOM('<!doctype html><html><head></head><body></body></html>');
  const style = dom.window.document.createElement('style');
  style.textContent = css;
  dom.window.document.head.appendChild(style);
  const selectors = [...dom.window.document.styleSheets[0].cssRules]
    .map((r) => r.selectorText)
    .filter(Boolean);

  // 1) brace balance
  let depth = 0;
  for (const ch of css) {
    if (ch === '{') depth++;
    if (ch === '}') depth--;
    if (depth < 0) break;
  }
  ok(depth === 0, `style.css braces balance (depth=${depth})`);

  // 2) no selector is polluted by a // comment (CSS has none)
  const polluted = selectors.filter((s) => s.includes('//'));
  ok(polluted.length === 0, `no selector is polluted by a // comment (${polluted.length})`,
    { polluted: polluted.slice(0, 3) });

  // 3) critical rules must really be in the sheet -- one typo deletes a rule
  const critical = [
    '.kv', '.kv>i', '.kv strong', '.kv p',
    '.msg', '.msg .text', '.alarm', '.shape',
    '[data-state="verified"]', 'p.no-margin',
  ];
  const missing = critical.filter((want) => !selectors.some((s) => s.replace(/\s+/g, '').includes(want.replace(/\s+/g, ''))));
  ok(missing.length === 0, `every critical rule parsed (${selectors.length} rules)`, { missing });

  // 4) .kv must be a horizontal flex container -- it is the integrity panel's skeleton
  const kv = selectors.find((s) => s === '.kv');
  ok(kv === '.kv', 'the .kv selector is clean (not eaten by the preceding line)', { found: kv });
}

// ── Result ─────────────────────────────────────────────────────────

console.log(`\n${'\u2500'.repeat(52)}`);
console.log(failed === 0 ? `\u2705 full smoke passed  ${passed} checks` : `\u274c ${failed} failed / ${passed} passed`);
process.exit(failed === 0 ? 0 : 1);
