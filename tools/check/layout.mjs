// check/layout.mjs —— 几何实测：横向溢出、越界裁剪、内容贴边、触摸目标。
// 直接冲着「安全区 / 碰到边界 / 移动端适配」这个问题写的。

import { VIEWPORTS, TABS, session, openDemo, shot } from '../lib/harness.mjs';

const CONTENT = [
  '#mark', '.brand', '#room', '#theme', '#views', '#views button',
  '#input', '#send', '.view.active article', '.msg', '.alarm',
];

const measure = (selectors) => {
  const vw = window.innerWidth;
  const els = [];
  for (const sel of selectors) {
    for (const el of document.querySelectorAll(sel)) {
      const r = el.getBoundingClientRect();
      if (!r.width && !r.height) continue;
      const cs = getComputedStyle(el);
      if (cs.display === 'none' || cs.visibility === 'hidden') continue;
      els.push({
        sel, tag: el.tagName.toLowerCase(),
        text: (el.textContent || '').trim().slice(0, 14),
        left: +r.left.toFixed(1), right: +r.right.toFixed(1), w: +r.width.toFixed(1), h: +r.height.toFixed(1),
      });
    }
  }
  return { vw, vh: window.innerHeight, docW: document.documentElement.scrollWidth, els };
};

export default async function layout(browser, base, c) {
  for (const vp of VIEWPORTS) {
    const { ctx, page } = await session(browser, vp);
    await openDemo(page, base);

    for (const tab of TABS) {
      if (tab.id !== 'chat') await page.click(`#views button[data-view="${tab.id}"]`);
      await page.waitForTimeout(400);

      const m = await page.evaluate(measure, CONTENT);
      await shot(page, `${vp.name}-${tab.id}.png`);
      const where = { vp: vp.name, tab: tab.id };

      c.ok(m.docW - m.vw <= 1, `[${vp.name}/${tab.id}] 无横向溢出 (docW=${m.docW} vw=${m.vw})`,
        { ...where, overflow: m.docW - m.vw });

      for (const el of m.els) {
        if (el.left < -1 || el.right > m.vw + 1) {
          c.fail('clipped', { ...where, ...el });
        } else if (el.left < 8 || el.right > m.vw - 8) {
          c.fail('flush-edge', { ...where, ...el, gapL: el.left, gapR: +(m.vw - el.right).toFixed(1) });
        }
        if (vp.isMobile && el.tag === 'button' && (el.h < 40 || el.w < 40)) {
          c.fail('small-target', { ...where, ...el });
        }
      }
    }

    // 安全区：确认左右真的留了边距
    const pad = await page.evaluate(() => {
      const h = getComputedStyle(document.querySelector('header.fixed'));
      const f = getComputedStyle(document.querySelector('footer.fixed'));
      const m = getComputedStyle(document.querySelector('main'));
      return { h: h.paddingLeft, f: f.paddingRight, m: m.paddingLeft };
    });
    c.ok(parseFloat(pad.h) >= 8 && parseFloat(pad.m) >= 8,
      `[${vp.name}] header/main 有左右安全边距 (h=${pad.h} m=${pad.m})`, { vp: vp.name, pad });

    await ctx.close();
  }
}
