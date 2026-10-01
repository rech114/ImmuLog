// SPDX-License-Identifier: Apache-2.0
// check/gossip.mjs -- three-node view comparison against the real Go binary.
//
// federation.mjs proves per-feed sync and rewrite detection. This check covers
// the one thing per-feed comparison is structurally blind to: a feed that one
// node serves and another never hears about.
//
// The scene is two disjoint relays that overlap on exactly one node:
//
//        hub-a                          hub-b
//      /   |   \                          |
//    alice  bob  carol  <-- carol also --> carol  dave
//
// Bob only ever talks to hub-a. Dave's feed lives on hub-b. So bob's git-sync
// picture is perfectly consistent -- every peer agrees about every feed bob
// knows -- and yet bob is missing a feed two other nodes can see. Nothing in
// `Sync` can report that: it groups claims by ref, and a ref nobody declares
// never enters its loop. Gossip compares the ref *sets*, so it can.
//
// Three nodes, not two, because two views disagreeing say only *that*
// something is wrong. The second independent reporter is what makes the
// missing feed a signal rather than one peer's claim.

import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { VIEWPORTS, session, shot } from '../lib/harness.mjs';
import {
  git, freePort, buildBinary, initRepo, startNode,
  waitHealthy, stopNode, publish,
} from '../lib/harness.mjs';

const SYNC = '400ms';
const GOSSIP = '400ms';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Poll until fn returns truthy, or throw with the last value seen. */
async function waitFor(label, fn, timeout = 25000) {
  const deadline = Date.now() + timeout;
  let last;
  while (Date.now() < deadline) {
    last = await fn();
    if (last) return last;
    await sleep(250);
  }
  throw new Error(`${label} did not happen within ${timeout}ms (last: ${JSON.stringify(last)})`);
}

const snapshotOf = async (base) => (await fetch(base + '/api/snapshot')).json();

export default async function gossip(browser, _base, c) {
  const work = await mkdtemp(join(tmpdir(), 'immulog-views-'));
  const bin = join(work, 'immulog');
  const hubA = join(work, 'hub-a.git');   // shared: alice, bob, carol
  const hubB = join(work, 'hub-b.git');   // shared: carol, dave
  const repos = { a: join(work, 'repoA'), b: join(work, 'repoB'), c: join(work, 'repoC'), d: join(work, 'repoD') };
  const nodes = {};

  try {
    // ── 1) Build and set the scene ───────────────────────────────
    try {
      buildBinary(bin);
      c.ok(true, 'go build succeeded');
    } catch (e) {
      c.ok(false, `go build failed: ${String(e.stderr || e).slice(0, 300)}`);
      return;
    }

    initRepo(hubA, 'hub-a');
    initRepo(hubB, 'hub-b');
    for (const [k, who] of [['a', 'alice'], ['b', 'bob'], ['c', 'carol'], ['d', 'dave']]) initRepo(repos[k], who);

    const ports = { a: await freePort(), b: await freePort(), c: await freePort(), d: await freePort() };
    const peer = (name, k) => `${name}=http://127.0.0.1:${ports[k]}`;

    nodes.a = startNode({
      bin, repo: repos.a, port: ports.a, remotes: [`shared=${hubA}`, `other=${hubB}`], who: 'alice',
      env: { IMMULOG_SYNC_INTERVAL: SYNC, IMMULOG_ANCHOR_INTERVAL: '2s' },
    });
    nodes.c = startNode({
      bin, repo: repos.c, port: ports.c, remotes: [`shared=${hubA}`, `other=${hubB}`], who: 'carol',
      env: { IMMULOG_SYNC_INTERVAL: SYNC, IMMULOG_ANCHOR_INTERVAL: '2s' },
    });
    nodes.d = startNode({
      bin, repo: repos.d, port: ports.d, remotes: [`other=${hubB}`], who: 'dave',
      env: { IMMULOG_SYNC_INTERVAL: SYNC, IMMULOG_ANCHOR_INTERVAL: '2s' },
    });
    // Bob talks to hub-a only. His gossip peers are alice and carol: the two
    // nodes that can see what hub-a never showed him.
    nodes.b = startNode({
      bin, repo: repos.b, port: ports.b, remotes: [`shared=${hubA}`], who: 'bob',
      env: {
        IMMULOG_SYNC_INTERVAL: SYNC,
        IMMULOG_ANCHOR_INTERVAL: '2s',
        IMMULOG_GOSSIP_INTERVAL: GOSSIP,
        IMMULOG_PEERS: [peer('alice', 'a'), peer('carol', 'c')].join(','),
      },
    });

    const health = {};
    for (const [k, n] of Object.entries(nodes)) {
      try {
        health[k] = await waitHealthy(n.base);
      } catch (e) {
        c.ok(false, `node ${k} did not start: ${e.message}\nlog:\n${n.log.slice(-600)}`);
        return;
      }
    }
    c.ok(new Set(Object.values(health).map((h) => h.feed)).size === 4, 'the four nodes hold four distinct feed identities');
    c.ok(health.b.gossip === 2, `bob has two gossip peers configured (${health.b.gossip})`);

    const post = async (base, body) => (await fetch(base + '/api/commit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ kind: 'msg', body }),
    })).json();

    // ── 2) Fill both relays ──────────────────────────────────────
    const aliceBody = 'gossip smoke ' + Math.random().toString(36).slice(2, 8);
    await post(nodes.a.base, aliceBody);
    await post(nodes.d.base, 'a message on the far side of the network');

    publish(hubA, repos.a);
    publish(hubB, repos.d);

    c.ok(git(hubA, 'for-each-ref', '--format=%(refname)', 'refs/feeds/').includes(health.a.feed),
      'hub-a carries alice');
    c.ok(git(hubB, 'for-each-ref', '--format=%(refname)', 'refs/feeds/').includes(health.d.feed),
      'hub-b carries dave');

    // ── 3) Bob syncs hub-a and stays ignorant of hub-b ───────────
    await waitFor('bob to receive alice over git sync', async () => {
      const s = await snapshotOf(nodes.b.base);
      return (s.refs ?? []).some((r) => r.name === `refs/feeds/${health.a.feed}`);
    });
    c.ok(true, 'bob received alice through ordinary git sync');

    // Carol and alice both see dave; alice reaches him through hub-b.
    await waitFor('carol to receive dave', async () => {
      const s = await snapshotOf(nodes.c.base);
      return (s.refs ?? []).some((r) => r.name === `refs/feeds/${health.d.feed}`);
    });
    await waitFor('alice to receive dave', async () => {
      const s = await snapshotOf(nodes.a.base);
      return (s.refs ?? []).some((r) => r.name === `refs/feeds/${health.d.feed}`);
    });
    c.ok(true, 'alice and carol both hold dave, through the relay bob cannot see');

    // ── 4) The blind spot: git sync reports nothing wrong ────────
    const bSnap = await snapshotOf(nodes.b.base);
    const bGitPeers = bSnap.peers ?? [];
    c.ok(bGitPeers.length === 1 && bGitPeers.every((p) => p.ok),
      'bob\'s git-sync picture is entirely consistent -- nothing for Sync to report');

    // ── 5) Gossip sees the missing feed, from two independent views ──
    const missing = await waitFor('bob to learn about dave from gossip', async () => {
      const s = await snapshotOf(nodes.b.base);
      const hit = (s.missingFeeds ?? []).find((m) => m.feed === `refs/feeds/${health.d.feed}`);
      return hit && hit.peers >= 2 ? hit : null;
    });
    c.ok(missing.peers === 2, `two independent peers report the feed bob cannot see (${missing.peers})`);

    const bGossip = (await snapshotOf(nodes.b.base)).gossip ?? [];
    c.ok(bGossip.length === 2 && bGossip.every((g) => g.ok),
      'both gossip peers agree with bob about every feed he does hold');
    c.ok(bGossip.every((g) => (g.missingHere ?? []).includes(`refs/feeds/${health.d.feed}`)),
      'each peer reports dave as a feed bob does not hold');
    c.ok(bGossip.every((g) => /knows feeds this node does not/.test(g.note ?? '')),
      'the peer note says what was learned rather than just "differs"');
    c.ok((await snapshotOf(nodes.b.base)).gossip.every((g) => g.digest && g.digest.length === 40),
      'each view arrives with the peer\'s digest, one request per peer');

    // ── 6) Read-only: gossip reports, it never promotes ──────────
    const bFeeds = git(repos.b, 'for-each-ref', '--format=%(refname)', 'refs/feeds/');
    const bWitness = git(repos.b, 'for-each-ref', '--format=%(refname)', 'refs/witness/');
    c.ok(!bFeeds.includes(health.d.feed), 'gossip did not write dave into refs/feeds/*');
    c.ok(!bWitness.includes(health.d.feed), 'gossip did not establish a witness for it either');
    c.ok(!(await snapshotOf(nodes.b.base)).refs.some((r) => r.name === `refs/feeds/${health.d.feed}`),
      'the local view is unchanged: promotion stays with sync, which owns the quarantine');

    // ── 7) The browser sees the same picture ─────────────────────
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e).slice(0, 200)));
    await page.goto(nodes.b.base + '/', { waitUntil: 'load' });
    await page.waitForSelector('#input', { timeout: 15000 });

    await page.click('#views button[data-view="peers"]');
    await page.waitForSelector('#peers .kv', { timeout: 10000 });

    const panel = await page.evaluate(() => ({
      rows: [...document.querySelectorAll('#peers .kv')].map((r) => r.textContent),
      missing: [...document.querySelectorAll('#missing-feeds .kv')].map((r) => r.textContent),
    }));
    // Match the tag, not the substring: a git peer's row shows its URL, and a
    // URL is allowed to contain the word "gossip".
    const tagged = panel.rows.filter((r) => r.includes('· gossip'));
    c.ok(tagged.length === 2,
      'the peers tab shows both gossip peers, tagged apart from the git remote',
      { panel });
    c.ok(panel.rows.some((r) => r.includes('agrees')), 'agreement is shown rather than assumed');
    c.ok(panel.missing.some((r) => r.includes('2 peers')),
      'the feed only peers can see is listed with its reporter count', { panel });

    await shot(page, 'gossip-peers.png');

    const real = errors.filter((e) => !/favicon|404/i.test(e));
    c.ok(real.length === 0, 'no uncaught JS errors throughout', { errors: real });

    await ctx.close();
  } finally {
    for (const n of Object.values(nodes)) stopNode(n);
    await rm(work, { recursive: true, force: true });
  }
}
