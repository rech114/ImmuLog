// check/shapes.mjs —— 视觉语汇真的渲染出来了吗。
//
// 两件事都容易静默失效：
//   · shape 是 mask-image: url(x.svg) —— SVG 取不到就退化成纯色方块
//   · 图标是 Material Symbols 连字字体 —— 字体没加载就会显示成 "cloud_done" 这串字
// 失效了页面不会报错，只会变丑。所以必须实测。

import { VIEWPORTS, session, openDemo, shot } from '../lib/harness.mjs';

const EXPECT_SHAPE = {
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
    iconFont: document.fonts.check('24px "Material Symbols Outlined"'),
  }));
  c.ok(cap.mask || cap.webkitMask, `支持 mask-image (标准=${cap.mask} webkit=${cap.webkitMask})`);
  c.ok(cap.safeArea, '支持 max() + env() 安全区写法');
  c.ok(cap.viewportFit, 'viewport meta 含 viewport-fit=cover', cap);
  c.ok(cap.iconFont, '图标字体已加载', cap);

  // ② 逐页签采集形状与图标（隐藏视图里的元素量不到，必须切过去）
  const shapesByState = {};
  const icons = [];
  const urls = new Set();

  for (const tab of ['chat', 'integrity', 'peers']) {
    await page.click(`#views button[data-view="${tab}"]`);
    await page.waitForTimeout(400);

    const got = await page.evaluate(() => {
      const shapes = [];
      const icons = [];
      for (const el of document.querySelectorAll('.shape')) {
        const r = el.getBoundingClientRect();
        if (!r.width && !r.height) continue;         // 隐藏视图，跳过
        const cs = getComputedStyle(el);
        shapes.push({
          state: el.dataset.state || (el.className.match(/shape (\S+)/)?.[1] ?? ''),
          mask: cs.maskImage || cs.webkitMaskImage || 'none',
          bg: cs.backgroundColor,
          w: +r.width.toFixed(1), h: +r.height.toFixed(1),
        });
      }
      for (const el of document.querySelectorAll('i')) {
        const r = el.getBoundingClientRect();
        if (!r.width && !r.height) continue;
        const cs = getComputedStyle(el);
        const fs = parseFloat(cs.fontSize);
        icons.push({
          name: (el.textContent || '').trim(),
          family: cs.fontFamily,
          fontSize: fs,
          w: +r.width.toFixed(1), h: +r.height.toFixed(1),
          // 连字没生效时，"cloud_done" 会被排成一行文字，宽度远超字号
          looksLikeText: r.width > fs * 1.9,
        });
      }
      return { shapes, icons };
    });

    for (const s of got.shapes) (shapesByState[s.state] ??= []).push(s);
    icons.push(...got.icons);
  }

  await shot(page, 'shapes-integrity.png');

  // ③ 形状
  c.ok(Object.keys(shapesByState).length >= 3,
    `出现 ${Object.keys(shapesByState).length} 种形状状态：${Object.keys(shapesByState).join(', ')}`,
    { states: Object.keys(shapesByState) });

  for (const [state, list] of Object.entries(shapesByState)) {
    const masks = new Set(list.map((x) => x.mask));
    c.ok(masks.size === 1, `状态 "${state}" 形状唯一（${list.length} 元素 / ${masks.size} 种 mask）`,
      { state, masks: [...masks] });

    const mask = list[0].mask;
    c.ok(mask !== 'none', `状态 "${state}" 有 mask`, { state, mask });
    if (EXPECT_SHAPE[state]) {
      c.ok(mask.includes(EXPECT_SHAPE[state]), `状态 "${state}" → ${EXPECT_SHAPE[state]}`,
        { state, mask, expect: EXPECT_SHAPE[state] });
    }
    c.ok(list[0].w > 0 && list[0].w <= 40, `状态 "${state}" 尺寸合理 (${list[0].w}×${list[0].h})`,
      { state, ...list[0] });

    for (const m of masks) {
      const u = m.match(/url\(["']?([^"')]+)["']?\)/)?.[1];
      if (u) urls.add(u);
    }
  }

  for (const u of urls) {
    try {
      const r = await fetch(u);
      c.ok(r.ok, `形状 SVG 可达 ${u.split('/').pop()} (HTTP ${r.status})`, { url: u, status: r.status });
    } catch (e) {
      c.ok(false, `形状 SVG 可达 ${u}`, { url: u, error: String(e).slice(0, 120) });
    }
  }

  const retracted = shapesByState.retracted?.[0];
  const verified = shapesByState.verified?.[0];
  if (retracted && verified) {
    c.ok(retracted.bg !== verified.bg, `撤回与已验签颜色不同 (${retracted.bg} vs ${verified.bg})`,
      { retracted: retracted.bg, verified: verified.bg });
  }

  // ④ 图标
  const uniqIcons = [...new Map(icons.map((i) => [i.name, i])).values()];
  c.ok(uniqIcons.length > 0, `找到 ${uniqIcons.length} 个图标：${uniqIcons.map((i) => i.name).join(', ')}`);
  const asText = icons.filter((i) => i.looksLikeText);
  c.ok(asText.length === 0, '所有图标都渲染成字形，没有退化成文字', { asText });
  for (const i of uniqIcons) {
    c.ok(
      i.family.includes('Material Symbols'),
      `图标 "${i.name}" 使用 Material Symbols 字体（${i.family.split(',')[0]}）`,
      { icon: i },
    );
  }

  await ctx.close();
}
