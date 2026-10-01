// SPDX-License-Identifier: Apache-2.0
// check/shapes.mjs -- did the visual vocabulary actually render?
//
// Two things fail silently:
//   - a shape is mask-image: url(x.svg) -- with the SVG unreachable it
//     degrades into a solid square
//   - icons are the Material Symbols ligature font -- with the font unloaded
//     you get the literal string "cloud_done"
// Neither throws. The page just gets ugly. So it has to be measured.

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

  // 1) Capability probe
  const cap = await page.evaluate(() => ({
    mask: CSS.supports('mask-image', 'url(x.svg)'),
    webkitMask: CSS.supports('-webkit-mask-image', 'url(x.svg)'),
    safeArea: CSS.supports('padding-left', 'max(8px, env(safe-area-inset-left))'),
    viewportFit: document.querySelector('meta[name=viewport]')?.content.includes('viewport-fit=cover') ?? false,
    iconFont: document.fonts.check('24px "Material Symbols Outlined"'),
  }));
  c.ok(cap.mask || cap.webkitMask, `mask-image is supported (standard=${cap.mask} webkit=${cap.webkitMask})`);
  c.ok(cap.safeArea, 'max() + env() safe-area syntax is supported');
  c.ok(cap.viewportFit, 'the viewport meta includes viewport-fit=cover', cap);
  c.ok(cap.iconFont, 'the icon font is loaded', cap);

  // 2) Collect shapes and icons tab by tab (elements in a hidden view measure
  //    as zero, so each tab has to be activated)
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
        if (!r.width && !r.height) continue;         // hidden view, skip
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
          // when the ligature fails, "cloud_done" is laid out as words and is
          // far wider than the font size
          looksLikeText: r.width > fs * 1.9,
        });
      }
      return { shapes, icons };
    });

    for (const s of got.shapes) (shapesByState[s.state] ??= []).push(s);
    icons.push(...got.icons);
  }

  await shot(page, 'shapes-integrity.png');

  // 3) Shapes
  c.ok(Object.keys(shapesByState).length >= 3,
    `${Object.keys(shapesByState).length} shape states seen: ${Object.keys(shapesByState).join(', ')}`,
    { states: Object.keys(shapesByState) });

  for (const [state, list] of Object.entries(shapesByState)) {
    const masks = new Set(list.map((x) => x.mask));
    c.ok(masks.size === 1, `state "${state}" has one shape (${list.length} elements / ${masks.size} masks)`,
      { state, masks: [...masks] });

    const mask = list[0].mask;
    c.ok(mask !== 'none', `state "${state}" has a mask`, { state, mask });
    if (EXPECT_SHAPE[state]) {
      c.ok(mask.includes(EXPECT_SHAPE[state]), `state "${state}" -> ${EXPECT_SHAPE[state]}`,
        { state, mask, expect: EXPECT_SHAPE[state] });
    }
    c.ok(list[0].w > 0 && list[0].w <= 40, `state "${state}" has a sane size (${list[0].w}x${list[0].h})`,
      { state, ...list[0] });

    for (const m of masks) {
      const u = m.match(/url\(["']?([^"')]+)["']?\)/)?.[1];
      if (u) urls.add(u);
    }
  }

  for (const u of urls) {
    try {
      const r = await fetch(u);
      c.ok(r.ok, `shape SVG reachable: ${u.split('/').pop()} (HTTP ${r.status})`, { url: u, status: r.status });
    } catch (e) {
      c.ok(false, `shape SVG reachable: ${u}`, { url: u, error: String(e).slice(0, 120) });
    }
  }

  const retracted = shapesByState.retracted?.[0];
  const verified = shapesByState.verified?.[0];
  if (retracted && verified) {
    c.ok(retracted.bg !== verified.bg, `retracted and verified differ in colour (${retracted.bg} vs ${verified.bg})`,
      { retracted: retracted.bg, verified: verified.bg });
  }

  // 4) Icons
  const uniqIcons = [...new Map(icons.map((i) => [i.name, i])).values()];
  c.ok(uniqIcons.length > 0, `found ${uniqIcons.length} icons: ${uniqIcons.map((i) => i.name).join(', ')}`);
  const asText = icons.filter((i) => i.looksLikeText);
  c.ok(asText.length === 0, 'every icon rendered as a glyph, none degraded into words', { asText });
  for (const i of uniqIcons) {
    c.ok(
      i.family.includes('Material Symbols'),
      `icon "${i.name}" uses the Material Symbols font (${i.family.split(',')[0]})`,
      { icon: i },
    );
  }

  await ctx.close();
}
