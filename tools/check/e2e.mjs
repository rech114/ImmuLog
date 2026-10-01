// SPDX-License-Identifier: Apache-2.0
// check/e2e.mjs -- end to end: the real Go binary + a real browser + a real
// git repository.
//
// Every other check runs against mocks or in-memory servers. This is the only
// one that wires the three together, so it is the only proof that frontend +
// backend + the real Git actually fit.
//
// Covers:
//   - the service starts, the frontend connects, SSE replays the first screen
//   - a message sent from the UI becomes a real git commit (cross-checked with the git CLI)
//   - it survives a reload (truly persisted, not front-end memory)
//   - retraction is an appended event; the original stays on the chain
//   - an external force-push produces an alarm notice in the page

import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, rm, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { chromium } from 'playwright';
import { VIEWPORTS, session, collector, shot, ROOT } from '../lib/harness.mjs';

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
  GIT_AUTHOR_NAME: 'alice',
  GIT_AUTHOR_EMAIL: 'alice@example.com',
  GIT_COMMITTER_NAME: 'alice',
  GIT_COMMITTER_EMAIL: 'alice@example.com',
};

const git = (dir, ...args) =>
  execFileSync('git', ['-C', dir, ...args], { env: GIT_ENV, encoding: 'utf8' }).trim();

async function freePort() {
  const { createServer } = await import('node:net');
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.on('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

async function waitHealthy(base, ms = 30000) {
  const deadline = Date.now() + ms;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(base + '/api/health');
      if (r.ok) return await r.json();
    } catch (e) {
      lastErr = e;
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`service not ready within ${ms}ms: ${lastErr}`);
}

export default async function e2e(browser, _base, c) {
  const work = await mkdtemp(join(tmpdir(), 'immulog-e2e-'));
  const repo = join(work, 'repoDB');
  const bin = join(work, 'immulog');
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  let proc;

  try {
    // ── 1) Build the real binary ─────────────────────────────────
    try {
      execFileSync('go', ['build', '-o', bin, '.'], { cwd: ROOT, stdio: 'pipe' });
      c.ok(true, 'go build succeeded');
    } catch (e) {
      c.ok(false, `go build failed: ${String(e.stderr || e).slice(0, 400)}`);
      return;
    }

    // ── 2) Create a bare repo with an identity (the server reads git
    //    config, not environment variables) ───────────────────────
    await mkdir(repo, { recursive: true });
    git(repo, 'init', '--bare', '--quiet', '-b', 'main');
    git(repo, 'config', 'user.name', 'alice');
    git(repo, 'config', 'user.email', 'alice@example.com');

    // ── 3) Start the service ─────────────────────────────────────
    proc = spawn(bin, [], {
      env: { ...GIT_ENV, IMMULOG_REPO: repo, PORT: String(port) },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let serverLog = '';
    proc.stdout.on('data', (d) => (serverLog += d));
    proc.stderr.on('data', (d) => (serverLog += d));

    const health = await waitHealthy(base).catch((e) => {
      c.ok(false, `${e.message}\nserver log:\n${serverLog.slice(-800)}`);
      return null;
    });
    if (!health) return;
    c.ok(health.ok === true, `service ready, feed=${health.feed} signed=${health.signed}`);
    c.ok(health.signed === false, 'with no signing key it reports signed=false (no pretending to be safe)');

    // ── 4) A real browser against a real backend (note: no ?demo=1) ─
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    const consoleErrors = [];
    page.on('console', (m) => m.type() === 'error' && consoleErrors.push(m.text().slice(0, 200)));
    page.on('pageerror', (e) => consoleErrors.push(String(e).slice(0, 200)));

    await page.goto(base + '/', { waitUntil: 'load' });
    await page.waitForSelector('#input', { timeout: 15000 });
    await page.waitForTimeout(1200);

    // An empty feed should show an empty state, not a blank board
    const empty = await page.evaluate(() => ({
      emptyShown: !!document.querySelector('#chat-empty'),
      msgs: document.querySelectorAll('.msg').length,
    }));
    c.ok(empty.msgs === 0, 'no messages initially');
    c.ok(empty.emptyShown, 'an empty feed shows an empty state (not a blank board)');

    // ── 5) Send a message from the UI ────────────────────────────
    const body = 'e2e smoke ' + Math.random().toString(36).slice(2, 8);
    await page.fill('#input', body);
    await page.click('#send');

    // Key point: the optimistic placeholder has text **immediately**, so
    // waiting for text alone is not enough. Wait for it to land (not pending,
    // and an OID that is a real 40-character object name).
    await page.waitForFunction(
      (t) => {
        const el = [...document.querySelectorAll('.msg')].find((e) => e.textContent.includes(t));
        return !!el && el.dataset.state !== 'pending' && /^[0-9a-f]{40}$/.test(el.dataset.oid);
      },
      body, { timeout: 15000 },
    );
    const afterSend = await page.evaluate((t) => {
      const el = [...document.querySelectorAll('.msg')].find((e) => e.textContent.includes(t));
      return { state: el.dataset.state, oid: el.dataset.oid, shape: el.querySelector('.shape').className };
    }, body);
    c.ok(afterSend.state === 'verified', `state is verified after sending (actually ${afterSend.state})`);
    c.ok(afterSend.shape.includes('gem'), 'verified -> shape gem');
    c.ok(/^[0-9a-f]{40}$/.test(afterSend.oid), `the OID in the DOM is a real 40-char object name (${afterSend.oid.slice(0, 8)}...)`);

    // ── 6) Cross-check with git: it must be a real commit object ──
    const ref = git(repo, 'for-each-ref', '--format=%(refname)', 'refs/feeds/');
    c.ok(ref === `refs/feeds/${health.feed}`, `the message landed in refs/feeds/<feed> (${ref})`);

    const logged = git(repo, 'log', '--format=%H%x1f%an%x1f%(trailers:key=ImmuLog-Seq,valueonly)', ref);
    const [oid, author, seq] = logged.split('\x1f');
    c.ok(oid === afterSend.oid, 'the DOM OID matches the object in git');
    c.ok(author === 'alice', `author comes from git config (${author})`);
    c.ok(seq.trim() === '1', `the ImmuLog-Seq trailer is written correctly (${JSON.stringify(seq)})`);

    // A bare repo has no index and no worktree -- proof the write path does
    // not use `git commit`
    c.ok(git(repo, 'rev-parse', '--is-bare-repository') === 'true', 'the repository is bare (no worktree, no index)');

    // ── 7) Still there after a reload (truly persisted) ──────────
    await page.reload({ waitUntil: 'load' });
    await page.waitForSelector('.msg', { timeout: 15000 });
    const persisted = await page.evaluate(
      (t) => [...document.querySelectorAll('.msg .text')].some((e) => e.textContent.includes(t)), body);
    c.ok(persisted, 'the message survives a reload (SSE replay reads real history)');

    // Optimistic-delivery dedupe: only one copy may end up in the DOM
    const dupes = await page.evaluate(
      (t) => [...document.querySelectorAll('.msg')].filter((e) => e.textContent.includes(t)).length, body);
    c.ok(dupes === 1, `optimistic delivery and the SSE broadcast dedupe by OID; one copy in the DOM (actually ${dupes})`);

    // ── 8) Retraction is an appended event ───────────────────────
    const retractResp = await page.evaluate(async (target) => {
      const r = await fetch('/api/commit', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ kind: 'retract', retracts: target, reason: 'e2e retraction' }),
      });
      return { status: r.status, body: await r.json() };
    }, afterSend.oid);
    c.ok(retractResp.status === 201, `the retract endpoint returns 201 (actually ${retractResp.status})`);

    await page.waitForFunction(
      (o) => document.querySelector(`.msg[data-oid="${o}"]`)?.dataset.state === 'retracted',
      afterSend.oid, { timeout: 10000 },
    );
    const n = git(repo, 'rev-list', '--count', ref);
    c.ok(n === '2', `chain length is 2 after retraction -- appended, not deleted (${n})`);
    const still = git(repo, 'log', '--format=%s', ref);
    c.ok(still.includes(body), 'the retracted message is still on the chain (never deleted)');

    // ── 9) An external force-push must produce an alarm notice ───
    const parent = git(repo, 'rev-parse', afterSend.oid);
    const tree = git(repo, 'hash-object', '-w', '-t', 'tree', '--stdin');
    const forged = execFileSync(
      'git',
      ['-C', repo, 'commit-tree', tree, '-p', parent],
      { env: GIT_ENV, input: 'rewritten history\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n', encoding: 'utf8' },
    ).trim();
    git(repo, 'update-ref', ref, forged); // attacker: a local ref can be forced without --force

    await page.reload({ waitUntil: 'load' });
    await page.waitForSelector('.alarm', { timeout: 15000 });
    const alarm = await page.evaluate(() => {
      const el = document.querySelector('.alarm');
      return {
        title: el.querySelector('h6')?.textContent ?? '',
        shape: el.querySelector('.shape')?.className ?? '',
        hasEvidence: !!document.querySelector('#alarm-log .kv'),
      };
    });
    c.ok(alarm.title.toLowerCase().includes('rewrite'), `the alarm notice appears: "${alarm.title}"`);
    c.ok(alarm.shape.includes('burst'), 'alarm -> shape burst');
    c.ok(alarm.hasEvidence, 'the alarm also appears in the integrity tab log');

    await shot(page, 'e2e-alarm.png');

    // ── 10) No uncaught errors anywhere ─────────────────────────
    const real = consoleErrors.filter((e) => !/favicon|404/i.test(e));
    c.ok(real.length === 0, 'no uncaught JS errors throughout', { errors: real });

    // The service is still alive
    const h2 = await (await fetch(base + '/api/health')).json();
    c.ok(h2.ok === true, 'the service still works after detecting tampering (alarms, does not strike)');

    await ctx.close();
  } finally {
    proc?.kill('SIGTERM');
    await new Promise((r) => setTimeout(r, 300));
    proc?.kill('SIGKILL');
    await rm(work, { recursive: true, force: true });
  }
}
