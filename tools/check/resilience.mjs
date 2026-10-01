// check/resilience.mjs —— 依赖挂掉时前端还活不活得下去。
//
// 这个项目宣称「零依赖单二进制」，但那说的是后端。前端目前仍从 CDN 取
// Beer CSS 和图标字体 —— 一旦取不到会怎样，必须实测，不能靠猜。

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
  // ① Beer 的 JS 挂掉 —— 检查我们的降级逻辑会不会变成无限轮询
  {
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    await page.addInitScript(countTimers);
    await page.route('**/beer.min.js', (r) => r.abort());
    await page.goto(`${base}/?demo=1`);
    await page.waitForSelector('.msg', { timeout: 25000 });

    // 要证的不是"重试得少"，而是"重试会停"——取两个时间点对比
    await page.waitForTimeout(2500);
    const early = await page.evaluate(() => window.__t['120'] ?? 0);
    await page.waitForTimeout(3000);
    const late = await page.evaluate(() => window.__t['120'] ?? 0);
    const counts = await page.evaluate(() => window.__t);

    c.ok(late === early, `Beer JS 不可用时有限重试后停止（2.5s=${early} 次 → 5.5s=${late} 次）`,
      { early, late });
    c.ok(late > 0 && late <= 20, `重试有上限且不是零次（${late}）`, { late, counts });

    const alive = await page.evaluate(() => ({
      msgs: document.querySelectorAll('.msg').length,
      hasInput: !!document.querySelector('#input'),
    }));
    c.ok(alive.msgs > 0 && alive.hasInput, 'Beer JS 挂掉后核心功能仍在（消息仍上屏）', alive);
    await ctx.close();
  }

  // ② Beer 的 CSS 挂掉 —— 应该只是丑，不该白屏
  {
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    await page.route('**/beer.min.css', (r) => r.abort());
    await page.goto(`${base}/?demo=1`);
    await page.waitForSelector('.msg', { timeout: 25000 });
    await page.waitForTimeout(1500);
    const n = await page.evaluate(() => document.querySelectorAll('.msg').length);
    c.ok(n > 0, `Beer CSS 挂掉仍可用，不白屏（${n} 条消息）`, { msgs: n });
    await ctx.close();
  }

  // ③ 写接口 500 —— 错误必须暴露给用户
  {
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    await page.route('**/beer.min.js', (r) => r.abort());
    await page.route('**/api/commit', (r) => r.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"internal"}' }));
    await page.goto(`${base}/`);
    await page.waitForTimeout(600);
    await page.fill('#input', '会失败的发送');
    await page.click('#send');
    await page.waitForTimeout(900);

    const s = await page.evaluate(() => ({
      toast: document.querySelector('#toast')?.classList.contains('active'),
      text: document.querySelector('#toast-text')?.textContent ?? '',
      unverified: document.querySelectorAll('.msg[data-state="unverified"]').length,
    }));
    c.ok(s.toast, `写接口 500 时给出提示（"${s.text}"）`, s);
    c.ok(s.unverified === 1, '失败的消息被标记为未验签，不冒充成功', s);
    await ctx.close();
  }
}
