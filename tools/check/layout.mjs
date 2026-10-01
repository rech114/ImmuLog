// SPDX-License-Identifier: Apache-2.0
// check/layout.mjs -- measured geometry: horizontal overflow, clipping,
// content flush to the edges, touch targets.
// Written directly against the "safe area / hitting the edges / mobile fit" problem.

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

      c.ok(m.docW - m.vw <= 1, `[${vp.name}/${tab.id}] no horizontal overflow (docW=${m.docW} vw=${m.vw})`,
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

    // Beer's global sibling-spacing rule once pushed <p> apart by 1rem; this
    // is the regression guard
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
    c.ok(gaps.length > 0, `[${vp.name}] the integrity panel has measurable key/value rows (${gaps.length})`);
    const maxGap = gaps.length ? Math.max(...gaps) : 0;
    c.ok(maxGap <= 6, `[${vp.name}] key/value heading and subtitle are tight (max ${maxGap}px)`,
      { vp: vp.name, gaps, maxGap });

    // .kv must be a horizontal flex container -- it is the whole skeleton of
    // the integrity panel
    const kv = await page.evaluate(() => {
      const el = document.querySelector('.kv');
      if (!el) return null;
      const cs = getComputedStyle(el);
      const r = el.getBoundingClientRect();
      return { display: cs.display, dir: cs.flexDirection, w: +r.width.toFixed(1), h: +r.height.toFixed(1) };
    });
    c.ok(kv && kv.display === 'flex',
      `[${vp.name}] .kv is a flex container (display=${kv?.display} ${kv?.dir})`, { vp: vp.name, kv });

    // Icon, heading and value must sit on one row: their vertical spans must
    // pairwise overlap. Comparing rounded `top` buckets was too fragile --
    // a two-line title pushed the buckets apart and produced false failures
    // while the row was in fact intact.
    const sameRow = await page.evaluate(() => {
      const el = document.querySelector('.kv');
      if (!el) return null;
      const nodes = [el.querySelector('i'), el.querySelector('strong'), el.querySelector('.chip')].filter(Boolean);
      const rects = nodes.map((n) => n.getBoundingClientRect());
      if (rects.length < 2) return null;
      let overlapAll = true;
      for (let i = 0; i < rects.length && overlapAll; i += 1) {
        for (let j = i + 1; j < rects.length; j += 1) {
          const overlap = Math.min(rects[i].bottom, rects[j].bottom) - Math.max(rects[i].top, rects[j].top);
          if (overlap <= 0) { overlapAll = false; break; }
        }
      }
      const minTop = Math.min(...rects.map((r) => r.top));
      const maxBottom = Math.max(...rects.map((r) => r.bottom));
      return {
        ok: overlapAll,
        spread: +(maxBottom - minTop).toFixed(1),
        h: +el.getBoundingClientRect().height.toFixed(1),
      };
    });
    c.ok(sameRow && sameRow.ok,
      `[${vp.name}] icon, heading and value share a row (vertical span ${sameRow?.spread}px)`, { vp: vp.name, sameRow });

    // Safe area: confirm the left and right margins really exist
    const pad = await page.evaluate(() => ({
      bar: getComputedStyle(document.querySelector('#bar > nav')).paddingLeft,
      dock: getComputedStyle(document.querySelector('#dock > nav')).paddingRight,
      wrap: getComputedStyle(document.querySelector('.wrap')).paddingLeft,
    }));
    const min = Math.min(parseFloat(pad.bar), parseFloat(pad.dock), parseFloat(pad.wrap));
    c.ok(min >= 8, `[${vp.name}] has left/right safe margins (bar=${pad.bar} dock=${pad.dock} wrap=${pad.wrap})`,
      { vp: vp.name, pad, min });

    await ctx.close();
  }
}
