// check/shapes.mjs —— 验证「形状即状态」真的渲染出来了。
//
// Beer CSS 的 shape 是 mask-image: url(x.svg)。若 SVG 取不到，
// 元素会退化成一个纯色方块 —— 整个形状语汇就静默失效了。
// 这一步专门盯住它。

import { VIEWPORTS, collector, session, openDemo, shot } from '../lib/harness.mjs';

// 消息状态的形状必须两两不同，且指向真实存在的 SVG
const EXPECT = {
  verified: 'gem.svg',
  retracted: 'slanted.svg',
  alarm: 'burst.svg',
  pending: 'loading-indicator.svg',
};

export default async function shapes(browser, base, c) {
  const { ctx, page } = await session(browser, VIEWPORTS[0]);
  await openDemo(page, base);

  // ① 能力探测
  const cap = await page.evaluate(() => ({
    mask: CSS.supports('mask-image', 'url(x.svg)'),
    webkitMask: CSS.supports('-webkit-mask-image', 'url(x.svg)'),
    safeArea: CSS.supports('padding-left', 'max(8px, env(safe-area-inset-left))'),
    viewportFit: document.querySelector('meta[name=viewport]')?.content.includes('viewport-fit=cover') ?? false,
  }));
  c.ok(cap.mask || cap.webkitMask, `浏览器支持 mask-image (标准=${cap.mask} webkit=${cap.webkitMask})`);
  c.ok(cap.safeArea, '支持 max() + env() 的安全区写法');
  c.ok(cap.viewportFit, 'viewport meta 含 viewport-fit=cover', cap);

  // ② 每种状态的实际计算值
  const actual = await page.evaluate(() => {
    const seen = {};
    for (const el of document.querySelectorAll('.shape')) {
      const state = el.dataset.state || (el.className.match(/shape (\S+)/)?.[1] ?? '');
      const cs = getComputedStyle(el);
      const mask = cs.maskImage || cs.webkitMaskImage || 'none';
      const r = el.getBoundingClientRect();
      (seen[state] ??= []).push({
        mask, bg: cs.backgroundColor,
        w: +r.width.toFixed(1), h: +r.height.toFixed(1),
        cls: el.className,
      });
    }
    return seen;
  });

  const states = Object.keys(actual);
  c.ok(states.length >= 3, `页面上出现 ${states.length} 种形状状态：${states.join(', ')}`, { states });

  const urls = new Set();
  for (const [state, list] of Object.entries(actual)) {
    const masks = new Set(list.map((x) => x.mask));
    c.ok(masks.size === 1, `状态 "${state}" 的形状唯一（${list.length} 个元素共 ${masks.size} 种 mask）`, { state, masks: [...masks] });

    const mask = list[0].mask;
    c.ok(mask !== 'none' && !mask.includes('none'), `状态 "${state}" 有 mask（${mask.slice(0, 70)}…）`, { state, mask });
    if (EXPECT[state]) {
      c.ok(mask.includes(EXPECT[state]), `状态 "${state}" → ${EXPECT[state]}`, { state, mask, expect: EXPECT[state] });
    }
    for (const m of masks) {
      const u = m.match(/url\(["']?([^"')]+)["']?\)/)?.[1];
      if (u) urls.add(u);
    }
    // 尺寸合理：图标不该是 Beer 默认的 3.5rem
    c.ok(list[0].w > 0 && list[0].w <= 40, `状态 "${state}" 尺寸合理 (${list[0].w}×${list[0].h})`, { state, ...list[0] });
  }

  // ③ 形状 SVG 真的可达（这就是静默失效点）
  for (const u of urls) {
    try {
      const r = await fetch(u, { method: 'GET' });
      c.ok(r.ok, `形状 SVG 可达 ${u.split('/').pop()} (HTTP ${r.status})`, { url: u, status: r.status });
    } catch (e) {
      c.ok(false, `形状 SVG 可达 ${u}`, { url: u, error: String(e).slice(0, 120) });
    }
  }

  // ④ 同一状态下不同元素颜色一致（撤回/告警应为 error 色）
  const retracted = actual.retracted?.[0];
  const verified = actual.verified?.[0];
  if (retracted && verified) {
    c.ok(retracted.bg !== verified.bg, `撤回与已验签颜色不同 (${retracted.bg} vs ${verified.bg})`,
      { retracted: retracted.bg, verified: verified.bg });
  }

  await shot(page, 'shapes-zoom.png');
  await ctx.close();
}
