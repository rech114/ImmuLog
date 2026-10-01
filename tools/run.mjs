// run.mjs —— 串行跑全部浏览器检查，汇总成 report.json，产物供 CI 回传。
//
// 串行是有意的：总共 5 个检查、约 40 秒，并发只会引入 flake，换不来任何东西。

import { mkdir, writeFile } from 'node:fs/promises';
import { join } from 'node:path';
import { chromium } from 'playwright';
import { serve, collector, OUT } from './lib/harness.mjs';

import layout from './check/layout.mjs';
import shapes from './check/shapes.mjs';
import resilience from './check/resilience.mjs';
import sse from './check/sse.mjs';
import a11y from './check/a11y.mjs';
import e2e from './check/e2e.mjs';

// 前五个跑在 mock / 内存服务上；e2e 打真实 Go 二进制（需要 PATH 里有 go）
const CHECKS = [layout, shapes, resilience, sse, a11y, e2e];

await mkdir(OUT, { recursive: true });

const { base, close } = await serve();
const browser = await chromium.launch();

const report = { ranAt: new Date().toISOString(), checks: [], issues: [] };
let totalPass = 0;
let totalFail = 0;

for (const mod of CHECKS) {
  const c = collector(mod.name);
  const t0 = Date.now();
  try {
    await mod(browser, base, c);
  } catch (e) {
    c.fail('crash', { message: String(e && e.stack ? e.stack : e).slice(0, 600) });
  }
  const ms = Date.now() - t0;

  totalPass += c.passed;
  totalFail += c.failed;
  report.checks.push({ name: mod.name, passed: c.passed, failed: c.failed, ms, issues: c.issues });
  report.issues.push(...c.issues);

  const mark = c.failed === 0 ? '\u2713' : '\u2717';
  console.log(`${mark} ${mod.name.padEnd(12)} ${String(c.passed).padStart(3)} 通过  ${String(c.failed).padStart(3)} 失败  (${ms}ms)`);
  for (const f of c.results.filter((r) => !r.pass)) console.log(`     \u2717 ${f.label}`);
}

await browser.close();
close();
await writeFile(join(OUT, 'report.json'), JSON.stringify(report, null, 2));

console.log('\n' + '\u2500'.repeat(56));
const byKind = report.issues.reduce((a, i) => ((a[i.kind] = (a[i.kind] ?? 0) + 1), a), {});
if (report.issues.length) {
  console.log('问题分布：');
  for (const [k, n] of Object.entries(byKind).sort((a, b) => b[1] - a[1])) console.log(`  ${k.padEnd(18)} ${n}`);
  console.log('');
}
for (const i of report.issues.slice(0, 25)) {
  if (i.kind === 'clipped') console.log(`  [裁剪] ${i.vp}/${i.tab} ${i.sel} "${i.text}" left=${i.left} right=${i.right}`);
  else if (i.kind === 'flush-edge') console.log(`  [贴边] ${i.vp}/${i.tab} ${i.sel} "${i.text}" 左${i.gapL}px 右${i.gapR}px`);
  else if (i.kind === 'small-target') console.log(`  [小目标] ${i.vp}/${i.tab} ${i.sel} "${i.text}" ${i.w}\u00d7${i.h}`);
  else if (i.kind === 'a11y') console.log(`  [无障碍] ${i.id} (${i.impact}) ${i.help} @ ${i.nodes?.join(', ')}`);
  else console.log(`  [${i.kind}] ${i.check} ${i.label ?? i.message ?? ''}`);
}
if (report.issues.length > 25) console.log(`  … 另有 ${report.issues.length - 25} 条，见 report.json`);

console.log(`\n合计 ${totalPass} 通过 / ${totalFail} 失败`);
process.exit(totalFail === 0 ? 0 : 1);
