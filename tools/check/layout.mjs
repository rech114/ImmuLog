// check/layout.mjs —— 几何实测：横向溢出、越界裁剪、内容贴边、触摸目标。
// 直接冲着「安全区 / 碰到边界 / 移动端适配」这个问题写的。

import { VIEWPORTS, TABS, session, openDemo, shot } from '../lib/harness.mjs';

const CONTENT = [
  '#mark', '.brand', '#room', '#theme', '#link',
  '#views button', '#input', '#send',
  '.view.active article', '.msg', '.alarm', '.kv', '.empty',
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

    // Beer 的全局兄弟间距规则曾把 <p> 顶开 1rem，这里加回归护栏
    await page.click('#views button[data-view="integrity"]');
    await page.waitForTimeout(400);
    const gaps = await page.evaluate(() => {
      const out = [];
      for (const kv of document.querySelectorAll('.kv')) {
        const s = kv.querySelector('strong');
        const p = kv.querySelector('p');
        if (!s || !p) continue;
        out.push(+(p.getBoundingClientRect().top - s.getBoundingClientRect().bottom).toFixed(1));
      }
      return out;
    });
    c.ok(gaps.length > 0, `[${vp.name}] 完整性页有可测的键值行 (${gaps.length} 行)`);
    const maxGap = gaps.length ? Math.max(...gaps) : 0;
    c.ok(maxGap <= 6, `[${vp.name}] 键值行标题↔副标题紧凑（最大 ${maxGap}px）`,
      { vp: vp.name, gaps, maxGap });

    // 安全区：确认左右真的留了边距
    const pad = await page.evaluate(() => ({
      bar: getComputedStyle(document.querySelector('#bar > nav')).paddingLeft,
      dock: getComputedStyle(document.querySelector('#dock > nav')).paddingRight,
      wrap: getComputedStyle(document.querySelector('.wrap')).paddingLeft,
    }));
    const min = Math.min(parseFloat(pad.bar), parseFloat(pad.dock), parseFloat(pad.wrap));
    c.ok(min >= 8, `[${vp.name}] 具备左右安全边距 (bar=${pad.bar} dock=${pad.dock} wrap=${pad.wrap})`,
      { vp: vp.name, pad, min });

    await ctx.close();
  }
}
