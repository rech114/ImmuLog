// SPDX-License-Identifier: Apache-2.0
// check/a11y.mjs -- accessibility scan plus keyboard operability.
//
// axe-core rather than @axe-core/playwright: the latter drags in
// @playwright/test, and we do not need a test runner. axe-core alone is
// enough -- inject one script and it runs.

import { createRequire } from 'node:module';
import { VIEWPORTS, session, openDemo } from '../lib/harness.mjs';

const require = createRequire(import.meta.url);
const AXE = require.resolve('axe-core/axe.min.js');

export default async function a11y(browser, base, c) {
  const { ctx, page } = await session(browser, VIEWPORTS[0]);
  await openDemo(page, base);

  // ── Keyboard operability: Tab reaches the input, Enter sends ────
  await page.click('#views button[data-view="chat"]');
  await page.waitForTimeout(300);

  const before = await page.evaluate(() => document.querySelectorAll('.msg').length);
  await page.keyboard.press('Tab');           // start walking from the body
  await page.evaluate(() => document.querySelector('#input').focus());
  await page.keyboard.type('sent by keyboard only');
  await page.keyboard.press('Enter');
  await page.waitForTimeout(1200);

  const after = await page.evaluate(() => ({
    n: document.querySelectorAll('.msg').length,
    last: [...document.querySelectorAll('.msg .text')].at(-1)?.textContent ?? '',
    input: document.querySelector('#input').value,
  }));
  c.ok(after.n === before + 1, 'Enter on the keyboard sends a message', { before, after });
  c.ok(after.last.includes('sent by keyboard only'), 'the keyboard-sent message has the right body', after);
  c.ok(after.input === '', 'the input is cleared after sending', after);

  // ── axe scan ───────────────────────────────────────────────────
  await page.addScriptTag({ path: AXE });
  const res = await page.evaluate(async () => {
    const r = await window.axe.run(document, { runOnly: ['wcag2a', 'wcag2aa'] });
    return r.violations.map((v) => ({
      id: v.id, impact: v.impact, help: v.help,
      nodes: v.nodes.slice(0, 3).map((n) => n.target.join(' ')),
    }));
  });

  c.ok(res.length === 0, `axe reports no WCAG A/AA violations (${res.length} kinds)`, { violations: res });
  for (const v of res) c.fail('a11y', { id: v.id, impact: v.impact, help: v.help, nodes: v.nodes });

  // ── Icon buttons must have an accessible name (a bare <i>light_mode</i>
  //    makes a screen reader read out the icon name) ──────────────
  const names = await page.evaluate(() => {
    const out = [];
    for (const b of document.querySelectorAll('button')) {
      const text = (b.textContent || '').trim();
      const aria = b.getAttribute('aria-label');
      if (!text && !aria) out.push(b.outerHTML.slice(0, 90));
      else if (text && !aria && /^[a-z_]+$/.test(text)) out.push(`icon name used as text: ${text}`);
    }
    return out;
  });
  c.ok(names.length === 0, 'every button has a readable accessible name', { unnamed: names });

  await ctx.close();
}
