// SPDX-License-Identifier: Apache-2.0
// check/resilience.mjs -- does the frontend survive its dependencies dying?
//
// The project claims "zero-dependency single binary", but that is about the
// backend. The frontend still pulls Beer CSS and the icon fonts from a CDN --
// what happens when they are unreachable has to be measured, not guessed.

import { VIEWPORTS, session } from '../lib/harness.mjs';

const countTimers = () => {
  window.__t = {};
  const orig = window.setTimeout;
  window.setTimeout = function (fn, ms, ...a) {
    window.__t[ms] = (window.__t[ms] || 0) + 1;
    return orig.call(window, fn, ms, ...a);
  };
};

export default async function resilience(browser, base, c) {
  // 1) Beer's JS is gone -- does our fallback degrade into an infinite poll?
  {
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    await page.addInitScript(countTimers);
    await page.route('**/beer.min.js', (r) => r.abort());
    await page.goto(`${base}/?demo=1`);
    await page.waitForSelector('.msg', { timeout: 25000 });

    // The property to prove is not "retries few times" but "retries stop":
    // sample at two points in time and compare
    await page.waitForTimeout(2500);
    const early = await page.evaluate(() => window.__t['120'] ?? 0);
    await page.waitForTimeout(3000);
    const late = await page.evaluate(() => window.__t['120'] ?? 0);
    const counts = await page.evaluate(() => window.__t);

    c.ok(late === early, `retries stop when Beer's JS is unavailable (2.5s=${early} -> 5.5s=${late})`,
      { early, late });
    c.ok(late > 0 && late <= 20, `retries are bounded and non-zero (${late})`, { late, counts });

    const alive = await page.evaluate(() => ({
      msgs: document.querySelectorAll('.msg').length,
      hasInput: !!document.querySelector('#input'),
    }));
    c.ok(alive.msgs > 0 && alive.hasInput, 'core functionality survives Beer JS dying (messages still render)', alive);
    await ctx.close();
  }

  // 2) Beer's CSS is gone -- that should be ugly, not a blank screen
  {
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    await page.route('**/beer.min.css', (r) => r.abort());
    await page.goto(`${base}/?demo=1`);
    await page.waitForSelector('.msg', { timeout: 25000 });
    await page.waitForTimeout(1500);
    const n = await page.evaluate(() => document.querySelectorAll('.msg').length);
    c.ok(n > 0, `still usable without Beer CSS, no blank screen (${n} messages)`, { msgs: n });
    await ctx.close();
  }

  // 3) the write endpoint returns 500 -- the error must reach the user
  {
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    await page.route('**/beer.min.js', (r) => r.abort());
    await page.route('**/api/commit', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"internal"}' }));
    await page.goto(`${base}/`);
    await page.waitForTimeout(600);
    await page.fill('#input', 'a send that will fail');
    await page.click('#send');
    await page.waitForTimeout(900);

    const s = await page.evaluate(() => ({
      toast: document.querySelector('#toast')?.classList.contains('active'),
      text: document.querySelector('#toast-text')?.textContent ?? '',
      unverified: document.querySelectorAll('.msg[data-state="unverified"]').length,
    }));
    c.ok(s.toast, `a 500 from the write endpoint is surfaced ("${s.text}")`, s);
    c.ok(s.unverified === 1, 'the failed message is marked unverified, not passed off as sent', s);
    await ctx.close();
  }
}
