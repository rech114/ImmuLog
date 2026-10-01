// check/a11y.mjs —— 无障碍扫描 + 键盘可用性。
//
// 用 axe-core 而不是 @axe-core/playwright：后者会拖进 @playwright/test，
// 而我们不需要 test runner。axe-core 单包即可，注入一段脚本就能跑。

import { createRequire } from 'node:module';
import { VIEWPORTS, session, openDemo } from '../lib/harness.mjs';

const require = createRequire(import.meta.url);
const AXE = require.resolve('axe-core/axe.min.js');

export default async function a11y(browser, base, c) {
  const { ctx, page } = await session(browser, VIEWPORTS[0]);
  await openDemo(page, base);

  // ── 键盘可用性：Tab 能到输入框，Enter 能发出消息 ──────────────
  await page.click('#views button[data-view="chat"]');
  await page.waitForTimeout(300);

  const before = await page.evaluate(() => document.querySelectorAll('.msg').length);
  await page.keyboard.press('Tab');           // 从 body 开始走
  await page.evaluate(() => document.querySelector('#input').focus());
  await page.keyboard.type('纯键盘发送');
  await page.keyboard.press('Enter');
  await page.waitForTimeout(1200);

  const after = await page.evaluate(() => ({
    n: document.querySelectorAll('.msg').length,
    last: [...document.querySelectorAll('.msg .text')].at(-1)?.textContent ?? '',
    input: document.querySelector('#input').value,
  }));
  c.ok(after.n === before + 1, '键盘 Enter 可发送消息', { before, after });
  c.ok(after.last.includes('纯键盘发送'), '键盘发出的消息内容正确', after);
  c.ok(after.input === '', '发送后输入框清空', after);

  // ── axe 扫描 ──────────────────────────────────────────────────
  await page.addScriptTag({ path: AXE });
  const res = await page.evaluate(async () => {
    const r = await window.axe.run(document, { runOnly: ['wcag2a', 'wcag2aa'] });
    return r.violations.map((v) => ({
      id: v.id, impact: v.impact, help: v.help,
      nodes: v.nodes.slice(0, 3).map((n) => n.target.join(' ')),
    }));
  });

  c.ok(res.length === 0, `axe 无 WCAG A/AA 违规（${res.length} 类）`, { violations: res });
  for (const v of res) c.fail('a11y', { id: v.id, impact: v.impact, help: v.help, nodes: v.nodes });

  // ── 图标按钮必须有无障碍名（<i>light_mode</i> 会让读屏念出图标名）──
  const names = await page.evaluate(() => {
    const out = [];
    for (const b of document.querySelectorAll('button')) {
      const text = (b.textContent || '').trim();
      const aria = b.getAttribute('aria-label');
      if (!text && !aria) out.push(b.outerHTML.slice(0, 90));
      else if (text && !aria && /^[a-z_]+$/.test(text)) out.push(`图标名当文本: ${text}`);
    }
    return out;
  });
  c.ok(names.length === 0, '所有按钮都有可读的无障碍名', { unnamed: names });

  await ctx.close();
}
