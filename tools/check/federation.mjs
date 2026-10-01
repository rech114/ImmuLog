// SPDX-License-Identifier: Apache-2.0
// check/federation.mjs -- two-node end to end: real multi-source sync and
// split-view detection.
//
// Every other check is single-node. This one starts two real processes sharing
// one relay repository, to verify three promises:
//   - someone else's messages sync over and become trusted state
//   - when the relay is rewritten, this node refuses the overwrite and raises an alarm
//   - the snapshot digest can be recomputed independently

import { execFileSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { VIEWPORTS, session, shot } from '../lib/harness.mjs';
import {
  GIT_ENV, git, gitIn, freePort, buildBinary, initRepo, startNode,
  waitHealthy, stopNode, publish,
} from '../lib/harness.mjs';

const SYNC_MS = '400ms';

/** Wait for a piece of text to appear in the page. */
async function waitForText(page, text, timeout = 20000) {
  await page.waitForFunction(
    (t) => [...document.querySelectorAll('.msg .text')].some((e) => e.textContent.includes(t)),
    text, { timeout },
  );
}

/** Is a an ancestor of b? git exits non-zero for "no", which is not an error. */
function isAncestor(dir, a, b) {
  try {
    execFileSync('git', ['-C', dir, 'merge-base', '--is-ancestor', a, b], { env: GIT_ENV, stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

export default async function federation(browser, _base, c) {
  const work = await mkdtemp(join(tmpdir(), 'immulog-fed-'));
  const bin = join(work, 'immulog');
  const hub = join(work, 'hub.git');
  const aRepo = join(work, 'repoA');
  const bRepo = join(work, 'repoB');
  let A, B;

  try {
    // ── 1) Build and set the scene ───────────────────────────────
    try {
      buildBinary(bin);
      c.ok(true, 'go build succeeded');
    } catch (e) {
      c.ok(false, `go build failed: ${String(e.stderr || e).slice(0, 300)}`);
      return;
    }
    initRepo(hub, 'hub');
    initRepo(aRepo, 'alice');
    initRepo(bRepo, 'bob');

    const remote = `hub=${hub}`;
    const portA = await freePort();
    const portB = await freePort();

    // ── 2) Node A sends messages ─────────────────────────────────
    A = startNode({ bin, repo: aRepo, port: portA, remotes: [remote], who: 'alice' });
    const healthA = await waitHealthy(A.base).catch((e) => {
      c.ok(false, `${e.message}\nnode A log:\n${A.log.slice(-600)}`);
      return null;
    });
    if (!healthA) return;

    const post = async (base, body) => (await fetch(base + '/api/commit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ kind: 'msg', body }),
    })).json();

    // Send two: the chain needs a fork point, or branching off the tip is just
    // a normal append and not a reference rewrite at all.
    const body = 'federation smoke ' + Math.random().toString(36).slice(2, 8);
    const first = await post(A.base, body);
    c.ok(/^[0-9a-f]{40}$/.test(first.oid || ''), `A sent the first message (${String(first.oid).slice(0, 8)}...)`);
    c.ok(first.seq === 1, `A's sequence starts at 1 (actually ${first.seq})`);

    const sent = await post(A.base, 'second message: leave a fork point on the chain');
    c.ok(sent.seq === 2, `A's sequence increments to 2 (actually ${sent.seq})`);

    // Make sure the relay carries A's feed (the local receive-pack is unusable;
    // see the README)
    publish(hub, aRepo);
    const hubRef = git(hub, 'for-each-ref', '--format=%(refname)', 'refs/feeds/');
    c.ok(hubRef === `refs/feeds/${healthA.feed}`, `the relay holds A's feed (${hubRef})`);

    // ── 3) Node B syncs it over ──────────────────────────────────
    B = startNode({
      bin, repo: bRepo, port: portB, remotes: [remote], who: 'bob',
      env: { IMMULOG_SYNC_INTERVAL: SYNC_MS, IMMULOG_ANCHOR_INTERVAL: '1s' },
    });
    const healthB = await waitHealthy(B.base).catch((e) => {
      c.ok(false, `${e.message}\nnode B log:\n${B.log.slice(-600)}`);
      return null;
    });
    if (!healthB) return;
    c.ok(healthB.feed !== healthA.feed, 'the two nodes have different identities');

    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e).slice(0, 200)));
    await page.goto(B.base + '/', { waitUntil: 'load' });
    await page.waitForSelector('#input', { timeout: 15000 });

    await waitForText(page, body);
    c.ok(true, 'B received A messages via sync');

    const got = await page.evaluate((t) => {
      const el = [...document.querySelectorAll('.msg')].find((e) => e.textContent.includes(t));
      return { state: el?.dataset.state, author: el?.querySelector('.who')?.textContent };
    }, body);
    c.ok(got.state === 'verified', `the synced message is verified (${got.state})`);
    c.ok(got.author === 'alice', `the author comes from the commit (${got.author})`);

    // B really turned it into trusted state, not just something on screen
    const aRef = `refs/feeds/${healthA.feed}`;
    const bTip = git(bRepo, 'rev-parse', aRef);
    c.ok(bTip === sent.oid, 'B placed the foreign feed into refs/feeds/*');
    const bWitness = git(bRepo, 'rev-parse', `refs/witness/${healthA.feed}`);
    c.ok(bWitness === sent.oid, 'B established a witness anchor for the foreign feed');

    // ── 4) Snapshot and peer state ───────────────────────────────
    const snap = await (await fetch(B.base + '/api/snapshot')).json();
    c.ok(/^[0-9a-f]{40}$/.test(snap.digest || ''), `B computes the snapshot digest independently (${String(snap.digest).slice(0, 8)}...)`);
    c.ok((snap.refs || []).some((r) => r.name === aRef), 'the snapshot includes the A feed');
    c.ok((snap.peers || []).length === 1 && snap.peers[0].ok, 'the peer is reported reachable and consistent');

    // The anchor chain
    await page.waitForTimeout(1500);
    const anchored = git(bRepo, 'for-each-ref', '--format=%(refname)', 'refs/anchors/');
    c.ok(anchored.includes('refs/anchors/'), `B produced an anchor (${anchored.split('\n')[0]})`);

    // ── 5) An attacker rewrites the A feed on the relay ──────────
    //
    // Branch from the **first** message, producing a sibling of the second --
    // so the forged tip and the local tip are not ancestors of one another,
    // which is what a genuine reference rewrite looks like.
    //
    // Build the chain in a clone and publish it: the forged commit must be
    // **reachable**, or the refs/feeds/* transfer will not carry it (the hub is
    // a different repository and cannot see dangling objects).
    const liarDir = join(work, 'liar.git');
    git(work, 'clone', '--bare', '--quiet', aRepo, liarDir);
    git(liarDir, 'config', 'user.name', 'alice');
    git(liarDir, 'config', 'user.email', 'alice@example.com');

    const tree = git(liarDir, 'hash-object', '-w', '-t', 'tree', '--stdin');
    const forged = gitIn(liarDir, 'rewritten history\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n',
      'commit-tree', tree, '-p', first.oid);
    c.ok(forged !== sent.oid, 'precondition: the forgery is a different object');
    git(liarDir, 'update-ref', aRef, forged);
    publish(hub, liarDir); // the relay is now rewritten
    c.ok(git(hub, 'rev-parse', aRef) === forged, 'the relay now points at the rewritten history');

    // Do not republish the honest chain -- that would immediately overwrite the
    // relay back to the honest version and B would never hit the contradiction
    await page.waitForSelector('.alarm', { timeout: 25000 });
    const alarm = await page.evaluate(() => ({
      title: document.querySelector('.alarm h6')?.textContent ?? '',
      shape: document.querySelector('.alarm .shape')?.className ?? '',
    }));
    c.ok(alarm.title.toLowerCase().includes('rewrite'), `B raises a rewrite alarm: "${alarm.title}"`);
    c.ok(alarm.shape.includes('burst'), 'the alarm shape is burst');

    // ── 6) The core verdict: local state must be intact ──────────
    const afterTip = git(bRepo, 'rev-parse', aRef);
    const afterWitness = git(bRepo, 'rev-parse', `refs/witness/${healthA.feed}`);
    c.ok(afterTip === sent.oid, 'B local copy refused the overwrite (still the honest tip)');
    c.ok(afterWitness === sent.oid, 'B witness anchor did not budge');

    c.ok(!isAncestor(bRepo, forged, afterTip), 'the forged commit is not in the B history');
    c.ok(isAncestor(bRepo, sent.oid, afterTip), 'the complete honest history is still held by B');

    const peers = (await (await fetch(B.base + '/api/snapshot')).json()).peers ?? [];
    c.ok(peers.length === 1 && peers[0].ok === false, 'the peer is marked inconsistent');

    await shot(page, 'federation-alarm.png');

    const real = errors.filter((e) => !/favicon|404/i.test(e));
    c.ok(real.length === 0, 'no uncaught JS errors throughout', { errors: real });

    await ctx.close();
  } finally {
    stopNode(A);
    stopNode(B);
    await rm(work, { recursive: true, force: true });
  }
}
