// lib/harness.mjs —— 浏览器检查的公共部件：静态服务、视口常量、断言收集。
// 只被 check/*.mjs 使用；不引入 @playwright/test。

import { createServer } from 'node:http';
import { readFile } from 'node:fs/promises';
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
  return {
    name,
    results,
    issues,
    ok(cond, label, detail) {
      results.push({ label, pass: !!cond });
      if (!cond) issues.push({ kind: 'assert', check: name, label, ...(detail ? { detail } : {}) });
      return !!cond;
    },
    fail(kind, detail) {
      issues.push({ kind, check: name, ...detail });
    },
    get passed() { return results.filter((r) => r.pass).length; },
    get failed() { return results.filter((r) => !r.pass).length; },
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
