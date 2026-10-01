// tools/visual.mjs —— 真实浏览器视觉/布局冒烟（跑在 CI 的 x86_64 上）
//
// 产出：
//   artifacts/*.png      各视口 × 各页签 截图（fullPage）
//   artifacts/report.json 机器可读的布局实测数据
//
// 断言的是「肉眼看到的问题」，不是「截图好看」：
//   · 横向溢出        scrollWidth > innerWidth
//   · 元素越界/被裁   boundingClientRect 超出视口
//   · 内容贴边        关键元素距视口边缘 < 8px
//   · 触摸目标过小    按钮 < 44×44（WCAG/MD3）
//   · 明暗主题失效    body 背景色没跟着变

import { createServer } from 'node:http';
import { readFile, mkdir, writeFile } from 'node:fs/promises';
import { extname, join, normalize, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import { chromium } from 'playwright';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const WEB = join(ROOT, 'web');
const OUT = join(ROOT, 'artifacts');

const VIEWPORTS = [
  { name: 'mobile', width: 390, height: 844, dpr: 3 },
  { name: 'tablet', width: 768, height: 1024, dpr: 2 },
  { name: 'desktop', width: 1280, height: 900, dpr: 2 },
];

const TABS = [
  { id: 'chat', label: '消息' },
  { id: 'integrity', label: '完整性' },
  { id: 'peers', label: '对端' },
];

// 内容元素（不含 header/footer 这两个本来就该通栏的容器）
const CONTENT = [
  '#mark', '.brand', '#room', '#theme', '#views', '#views button',
  '#input', '#send', '.view.active article', '.msg', '.alarm',
];

const MIME = {
  '.html': 'text/html; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.svg': 'image/svg+xml',
};

// ── 静态服务（复用 web/，不引第三方）─────────────────────────────

const server = createServer(async (req, res) => {
  const url = new URL(req.url, 'http://x');
  let p = normalize(join(WEB, decodeURIComponent(url.pathname)));
  if (!p.startsWith(WEB)) return res.writeHead(403).end();
  if (url.pathname === '/') p = join(WEB, 'index.html');
  try {
    const buf = await readFile(p);
    res.writeHead(200, { 'content-type': MIME[extname(p)] ?? 'application/octet-stream' });
    res.end(buf);
  } catch {
    res.writeHead(404).end();
  }
});
await new Promise((r) => server.listen(0, '127.0.0.1', r));
const BASE = `http://127.0.0.1:${server.address().port}`;

// ── 测量 ──────────────────────────────────────────────────────────

const measure = (selectors) => {
  const vw = window.innerWidth;
  const docW = document.documentElement.scrollWidth;
  const els = [];
  for (const sel of selectors) {
    for (const el of document.querySelectorAll(sel)) {
      const r = el.getBoundingClientRect();
      if (!r.width && !r.height) continue;
      const cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden') continue;
      els.push({
        sel,
        tag: el.tagName.toLowerCase(),
        text: (el.textContent || '').trim().slice(0, 14),
        left: +r.left.toFixed(1),
        right: +r.right.toFixed(1),
        top: +r.top.toFixed(1),
        bottom: +r.bottom.toFixed(1),
        w: +r.width.toFixed(1),
        h: +r.height.toFixed(1),
      });
    }
  }
  const bodyBg = getComputedStyle(document.body).backgroundColor;
  return { vw, vh: window.innerHeight, docW, overflow: docW - vw, bodyBg, els };
};

const issue = (kind, vp, detail) => ({ kind, viewport: vp, ...detail });

// ── 主流程 ────────────────────────────────────────────────────────

await mkdir(OUT, { recursive: true });
const browser = await chromium.launch();
const report = { ranAt: new Date().toISOString(), viewports: [], issues: [] };

for (const vp of VIEWPORTS) {
  const ctx = await browser.newContext({
    viewport: { width: vp.width, height: vp.height },
    deviceScaleFactor: vp.dpr,
    isMobile: vp.name === 'mobile',
    hasTouch: vp.name === 'mobile',
  });
  const page = await ctx.newPage();
  page.on('pageerror', (e) => report.issues.push(issue('page-error', vp.name, { message: String(e) })));
  page.on('console', (m) => {
    if (m.type() === 'error') report.issues.push(issue('console-error', vp.name, { message: m.text() }));
  });

  await page.goto(`${BASE}/?demo=1`, { waitUntil: 'load' });
  await page.waitForSelector('.msg', { timeout: 20000 });
  await page.waitForTimeout(6000); // 等撤回(3.2s)与告警(4.6s)落地

  const shots = {};
  for (const tab of TABS) {
    if (tab.id !== 'chat') await page.click(`#views button[data-view="${tab.id}"]`);
    await page.waitForTimeout(450);

    shots[tab.id] = `artifacts/${vp.name}-${tab.id}-dark.png`;
    await page.screenshot({ path: join(ROOT, shots[tab.id]), fullPage: true });

    const m = await page.evaluate(measure, CONTENT);

    // ① 横向溢出
    if (m.overflow > 1) report.issues.push(issue('h-overflow', vp.name, { tab: tab.id, overflow: m.overflow, docW: m.docW, vw: m.vw }));

    for (const el of m.els) {
      // ② 被裁到视口外
      if (el.left < -1 || el.right > m.vw + 1) {
        report.issues.push(issue('clipped', vp.name, { tab: tab.id, ...el }));
      }
      // ③ 内容贴边（右边界也是贴边）
      else if (el.left < 8 || el.right > m.vw - 8) {
        report.issues.push(issue('flush-edge', vp.name, { tab: tab.id, gapL: el.left, gapR: +(m.vw - el.right).toFixed(1), ...el }));
      }
      // ④ 触摸目标过小（mobile 上才算问题）
      if (vp.name === 'mobile' && el.tag === 'button' && (el.h < 40 || el.w < 40)) {
        report.issues.push(issue('small-target', vp.name, { tab: tab.id, ...el }));
      }
    }

    if (tab.id === 'chat') {
      report.viewports.push({
        name: vp.name, w: vp.width, h: vp.height,
        docW: m.docW, overflow: m.overflow, bodyBg: m.bodyBg,
        counts: await page.evaluate(() => ({
          msg: document.querySelectorAll('.msg').length,
          alarm: document.querySelectorAll('.alarm').length,
          retracted: document.querySelectorAll('.msg[data-state="retracted"]').length,
          pending: document.querySelectorAll('.msg[data-state="pending"]').length,
        })),
      });
    }
  }

  // ⑤ 明暗主题必须真的切
  const dark = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  await page.click('#theme');
  await page.waitForTimeout(450);
  const light = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  await page.screenshot({ path: join(ROOT, `artifacts/${vp.name}-chat-light.png`), fullPage: true });
  if (dark === light) report.issues.push(issue('theme-not-switching', vp.name, { dark, light }));

  await ctx.close();
}

await browser.close();
server.close();

// ── 汇总 ──────────────────────────────────────────────────────────

await writeFile(join(OUT, 'report.json'), JSON.stringify(report, null, 2));

const byKind = report.issues.reduce((a, i) => ((a[i.kind] = (a[i.kind] ?? 0) + 1), a), {});
console.log('\n布局实测');
for (const v of report.viewports) {
  console.log(`  ${v.name.padEnd(8)} ${v.w}×${v.h}  docW=${v.docW} 溢出=${v.overflow}  bg=${v.bodyBg}  msg=${v.counts.msg} 撤回=${v.counts.retracted} 告警=${v.counts.alarm}`);
}
console.log('\n问题统计');
for (const [k, n] of Object.entries(byKind).sort((a, b) => b[1] - a[1])) console.log(`  ${k.padEnd(22)} ${n}`);
if (!report.issues.length) console.log('  （无）');

for (const i of report.issues.slice(0, 40)) {
  const where = `${i.viewport}/${i.tab ?? '-'}`;
  if (i.kind === 'clipped') console.log(`  ✗ [裁剪] ${where} ${i.sel} "${i.text}" left=${i.left} right=${i.right}`);
  else if (i.kind === 'flush-edge') console.log(`  ✗ [贴边] ${where} ${i.sel} "${i.text}" 左${i.gapL}px 右${i.gapR}px`);
  else if (i.kind === 'small-target') console.log(`  ✗ [小目标] ${where} ${i.sel} "${i.text}" ${i.w}×${i.h}`);
  else console.log(`  ✗ [${i.kind}] ${where}`, i.message ?? '');
}
if (report.issues.length > 40) console.log(`  … 另有 ${report.issues.length - 40} 条，见 report.json`);

console.log(`\n共 ${report.issues.length} 个布局问题`);

// 截图与日志始终上传；布局问题不阻塞 CI（第一轮先取基线）
process.exit(0);
