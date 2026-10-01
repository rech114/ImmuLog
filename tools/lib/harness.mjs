// lib/harness.mjs —— 浏览器检查的公共部件：静态服务、视口常量、断言收集。
// 只被 check/*.mjs 使用；不引入 @playwright/test。

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
  { id: 'chat', label: '消息' },
  { id: 'integrity', label: '完整性' },
  { id: 'peers', label: '对端' },
];

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.json': 'application/json',
};

/** 极简静态服务。复用 node:http，不引依赖。routes 可挂额外端点（如 mock SSE）。 */
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

/** 一个视口 + 一个页面。 */
export async function session(browser, vp = VIEWPORTS[0]) {
  const ctx = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: vp.dpr,
    isMobile: !!vp.isMobile,
    hasTouch: !!vp.isMobile,
  });
  return { ctx, page: await ctx.newPage() };
}

/** 断言收集器：每条都记 pass/fail，最后统一汇总。 */
export function collector(name) {
  const results = [];
  const issues = [];
  let failures = 0;
  return {
    name,
    results,
    issues,
    ok(cond, label, detail) {
      results.push({ label, pass: !!cond });
      if (!cond) {
        failures += 1;
        issues.push({ kind: 'assert', check: name, label, ...(detail ? { detail } : {}) });
      }
      return !!cond;
    },
    // fail 必须计入失败 —— 否则「检查崩了」会被当成通过（CI 假绿）。
    fail(kind, detail) {
      failures += 1;
      issues.push({ kind, check: name, ...detail });
    },
    get passed() { return results.filter((r) => r.pass).length; },
    get failed() { return results.filter((r) => !r.pass).length + failures; },
  };
}

/** 捕获页面级错误（JS 异常 / console.error），供各检查复用。 */
export function watchErrors(page, c, label) {
  page.on('pageerror', (e) => c.fail('page-error', { where: label, message: String(e).slice(0, 300) }));
  page.on('console', (m) => {
    if (m.type() === 'error') c.fail('console-error', { where: label, message: m.text().slice(0, 300) });
  });
}

/** 打开 demo 页面并等到时间线有内容。 */
export async function openDemo(page, base, { waitMs = 6000 } = {}) {
  await page.goto(`${base}/?demo=1`, { waitUntil: 'load' });
  await page.waitForSelector('.msg', { timeout: 25000 });
  await page.waitForTimeout(waitMs);
}

export const shot = (page, file) => page.screenshot({ path: join(OUT, file), fullPage: true });

// ── 真实节点（端到端用）────────────────────────────────────────────
//
// 所有 git 调用都隔离到 /dev/null 配置，绝不碰开发机环境。

export const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
  GIT_AUTHOR_NAME: 'harness',
  GIT_AUTHOR_EMAIL: 'harness@example.com',
  GIT_COMMITTER_NAME: 'harness',
  GIT_COMMITTER_EMAIL: 'harness@example.com',
};

/** 直接在测试里跑 git —— 生产代码禁止 os/exec，测试不受此限。 */
export const git = (dir, ...args) =>
  execFileSync('git', ['-C', dir, ...args], { env: GIT_ENV, encoding: 'utf8' }).trim();

export const gitIn = (dir, input, ...args) =>
  execFileSync('git', ['-C', dir, ...args], { env: GIT_ENV, input, encoding: 'utf8' }).trim();

/** 要一个空闲端口。 */
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

/** 构建真实二进制；失败时抛出带 stderr 的错误。 */
export function buildBinary(outPath) {
  execFileSync('go', ['build', '-o', outPath, '.'], { cwd: ROOT, stdio: 'pipe' });
}

/** 建一个 bare 仓库并配好身份 —— 服务端只读 git 配置，不吃环境变量。 */
export function initRepo(dir, who) {
  mkdirSync(dir, { recursive: true });
  git(dir, 'init', '--bare', '--quiet', '-b', 'main');
  git(dir, 'config', 'user.name', who);
  git(dir, 'config', 'user.email', `${who}@example.com`);
}

/** 起一个节点进程，返回句柄。 */
export function startNode({ bin, repo, port, remotes = [], env = {}, who = 'node' }) {
  const proc = spawn(bin, [], {
    env: {
      ...GIT_ENV,
      GIT_AUTHOR_NAME: who,
      GIT_AUTHOR_EMAIL: `${who}@example.com`,
      GIT_COMMITTER_NAME: who,
      GIT_COMMITTER_EMAIL: `${who}@example.com`,
      IMMUTALK_REPO: repo,
      PORT: String(port),
      IMMUTALK_REMOTES: remotes.join(','),
      ...env,
    },
    stdio: ['ignore', 'pipe', 'pipe'],
  });
  const node = { proc, log: '', base: `http://127.0.0.1:${port}` };
  proc.stdout.on('data', (d) => (node.log += d));
  proc.stderr.on('data', (d) => (node.log += d));
  return node;
}

export async function waitHealthy(base, ms = 30000) {
  const deadline = Date.now() + ms;
  let last;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(base + '/api/health');
      if (r.ok) return await r.json();
    } catch (e) {
      last = e;
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`服务未在 ${ms}ms 内就绪：${last}`);
}

export function stopNode(node) {
  try {
    node?.proc?.kill('SIGTERM');
    node?.proc?.kill('SIGKILL');
  } catch { /* 已经退了 */ }
}

/**
 * 让 hub 拿到 src 的 feed。
 *
 * 走 fetch 而不是 push：本机 sandbox 的 receive-pack 不可用（见 README
 * 「已知的本机环境限制」）。CI 上节点自己的 push 已经完成，这一步是幂等的空操作。
 */
export function publish(hubDir, srcDir) {
  git(hubDir, 'fetch', '--quiet', srcDir, '+refs/feeds/*:refs/feeds/*');
}
