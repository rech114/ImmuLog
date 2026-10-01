// SPDX-License-Identifier: Apache-2.0
// lib/harness.mjs -- shared parts for the browser checks: static server,
// viewport constants, assertion collector.
// Used only by check/*.mjs; it does not pull in @playwright/test.

import { createServer } from 'node:http';
import { readFile, mkdir } from 'node:fs/promises';
import { mkdirSync } from 'node:fs';
import { spawn, execFileSync } from 'node:child_process';
import { extname, join, normalize, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

export const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');
export const WEB = join(ROOT, 'web');
export const OUT = join(ROOT, 'artifacts');

export const VIEWPORTS = [
  { name: 'mobile', width: 390, height: 844, dpr: 3, isMobile: true },
  { name: 'tablet', width: 768, height: 1024, dpr: 2, isMobile: false },
  { name: 'desktop', width: 1280, height: 900, dpr: 2, isMobile: false },
];

export const TABS = [
  { id: 'chat', label: 'messages' },
  { id: 'integrity', label: 'integrity' },
  { id: 'peers', label: 'peers' },
];

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.json': 'application/json',
};

/** A minimal static server on node:http, no dependencies. `routes` can mount extra endpoints (a mock SSE, say). */
export async function serve(dir = WEB, routes = {}) {
  const hits = new Map();
  const server = createServer(async (req, res) => {
    const url = new URL(req.url, 'http://x');
    hits.set(url.pathname, (hits.get(url.pathname) ?? 0) + 1);
    if (routes[url.pathname]) return routes[url.pathname](req, res, url);
    let p = normalize(join(dir, decodeURIComponent(url.pathname)));
    if (!p.startsWith(dir)) return res.writeHead(403).end();
    if (url.pathname === '/') p = join(dir, 'index.html');
    try {
      const buf = await readFile(p);
      res.writeHead(200, { 'content-type': MIME[extname(p)] ?? 'application/octet-stream' });
      res.end(buf);
    } catch {
      res.writeHead(404).end();
    }
  });
  await new Promise((r) => server.listen(0, '127.0.0.1', r));
  return { base: `http://127.0.0.1:${server.address().port}`, hits, close: () => server.close() };
}

/** One viewport plus one page. */
export async function session(browser, vp = VIEWPORTS[0]) {
  const ctx = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: vp.dpr,
    isMobile: !!vp.isMobile,
    hasTouch: !!vp.isMobile,
  });
  return { ctx, page: await ctx.newPage() };
}

/** Assertion collector: records pass/fail per item and aggregates at the end. */
export function collector(name) {
  const results = [];
  const issues = [];
  let failures = 0;
  return {
    name,
    results,
    issues,
    ok(cond, label, detail) {
      const pass = !!cond;
      results.push({ label, pass });
      // A failed assertion is already counted by `results`; incrementing a
      // second counter here made one failure report as two.
      if (!pass) issues.push({ kind: 'assert', check: name, label, ...(detail ? { detail } : {}) });
      return pass;
    },
    // fail must count as a failure -- otherwise "the check crashed" is recorded
    // as a pass (a falsely green CI).
    fail(kind, detail) {
      failures += 1;
      issues.push({ kind, check: name, ...detail });
    },
    get passed() { return results.filter((r) => r.pass).length; },
    get failed() { return results.filter((r) => !r.pass).length + failures; },
  };
}

/** Capture page-level errors (JS exceptions / console.error) for every check. */
export function watchErrors(page, c, label) {
  page.on('pageerror', (e) => c.fail('page-error', { where: label, message: String(e).slice(0, 300) }));
  page.on('console', (m) => {
    if (m.type() === 'error') c.fail('console-error', { where: label, message: m.text().slice(0, 300) });
  });
}

/** Open the demo page and wait until the timeline has content. */
export async function openDemo(page, base, { waitMs = 6000 } = {}) {
  await page.goto(`${base}/?demo=1`, { waitUntil: 'load' });
  await page.waitForSelector('.msg', { timeout: 25000 });
  await page.waitForTimeout(waitMs);
}

export const shot = (page, file) => page.screenshot({ path: join(OUT, file), fullPage: true });

// ── Real nodes (used by the end-to-end checks) ──────────────────────
//
// Every git call is isolated to /dev/null config; the development machine's
// environment is never touched.

export const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
  GIT_AUTHOR_NAME: 'harness',
  GIT_AUTHOR_EMAIL: 'harness@example.com',
  GIT_COMMITTER_NAME: 'harness',
  GIT_COMMITTER_EMAIL: 'harness@example.com',
};

// ── The node token (DESIGN §7.11, T2) ───────────────────────────────
//
// Every check that drives the real binary has to present it. It is a fixed
// value rather than a generated one because a node logs a generated token once
// and a test cannot read it back out of the log stream.

export const TEST_TOKEN = 'test-token-not-a-secret';

/** Headers for a direct API call from a check. */
export const authHeaders = (token = TEST_TOKEN) => ({ Authorization: `Bearer ${token}` });

/** Put the token into a browser context as the cookie the server would have set. */
export async function signIn(ctx, base, token = TEST_TOKEN) {
  await ctx.addCookies([
    { name: 'immulog_token', value: token, url: base, httpOnly: true, sameSite: 'Strict' },
  ]);
}

/** Run git directly from a test -- production code forbids os/exec, tests do not. */
export const git = (dir, ...args) =>
  execFileSync('git', ['-C', dir, ...args], { env: GIT_ENV, encoding: 'utf8' }).trim();

export const gitIn = (dir, input, ...args) =>
  execFileSync('git', ['-C', dir, ...args], { env: GIT_ENV, input, encoding: 'utf8' }).trim();

/** Ask the OS for a free port. */
export async function freePort() {
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

/** Build the real binary; throws with stderr attached on failure. */
export function buildBinary(outPath) {
  execFileSync('go', ['build', '-o', outPath, '.'], { cwd: ROOT, stdio: 'pipe' });
}

/** Create a bare repo with an identity -- the server reads git config, not environment variables. */
export function initRepo(dir, who) {
  mkdirSync(dir, { recursive: true });
  git(dir, 'init', '--bare', '--quiet', '-b', 'main');
  git(dir, 'config', 'user.name', who);
  git(dir, 'config', 'user.email', `${who}@example.com`);
}

/** Start a node process and return a handle. */
export function startNode({ bin, repo, port, remotes = [], env = {}, who = 'node' }) {
  const proc = spawn(bin, [], {
    env: {
      ...GIT_ENV,
      GIT_AUTHOR_NAME: who,
      GIT_AUTHOR_EMAIL: `${who}@example.com`,
      GIT_COMMITTER_NAME: who,
      GIT_COMMITTER_EMAIL: `${who}@example.com`,
      IMMULOG_REPO: repo,
      PORT: String(port),
      IMMULOG_REMOTES: remotes.join(','),
      // §7.11 T2: the HTTP surface is closed unless it is given a token. A
      // check that wants the old wide-open behaviour passes IMMULOG_OPEN=1.
      IMMULOG_TOKEN: TEST_TOKEN,
      ...env,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const node = { proc, log: '', base: `http://127.0.0.1:${port}`, token: env.IMMULOG_TOKEN ?? TEST_TOKEN };
  proc.stdout.on('data', (d) => (node.log += d));
  proc.stderr.on('data', (d) => (node.log += d));
  return node;
}

export async function waitHealthy(base, ms = 30000, token = TEST_TOKEN) {
  const deadline = Date.now() + ms;
  let last;
  while (Date.now() < deadline) {
    try {
      // Anonymous health answers with {ok} only; the token is needed for the
      // payload the checks actually assert on.
      const r = await fetch(base + '/api/health', { headers: authHeaders(token) });
      if (r.ok) return await r.json();
    } catch (e) {
      last = e;
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`service not ready within ${ms}ms: ${last}`);
}

export function stopNode(node) {
  try {
    node?.proc?.kill('SIGTERM');
    node?.proc?.kill('SIGKILL');
  } catch { /* already gone */ }
}

/**
 * Make the hub carry src's feeds.
 *
 * This fetches rather than pushes: the local sandbox's receive-pack is unusable
 * (see the README's "known local environment limits").
 *
 * On CI the node's own `pushLoop` DOES work, so it writes the same hub ref
 * concurrently and git reports "cannot lock ref ... but expected". That is an
 * ordinary write race, not an error: wait for it and retry.
 */
export function publish(hubDir, srcDir, tries = 6) {
  for (let i = 0; i < tries; i += 1) {
    try {
      git(hubDir, 'fetch', '--quiet', srcDir, '+refs/feeds/*:refs/feeds/*');
      return;
    } catch (e) {
      const msg = String(e.stderr || e.message || e);
      if (!/cannot lock ref|cannot lock|but expected/.test(msg)) throw e;
      sleepSync(200);
    }
  }
  // The last attempt does not catch, so the caller sees the real error
  git(hubDir, 'fetch', '--quiet', srcDir, '+refs/feeds/*:refs/feeds/*');
}

/** execFileSync is synchronous, so waiting here has to be synchronous too. */
export function sleepSync(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}
