// SPDX-License-Identifier: Apache-2.0
// run.mjs -- runs every browser check serially, summarises into report.json and
// leaves the artifacts for CI to upload.
//
// Serial is deliberate: five checks, about 40 seconds, and concurrency would
// only introduce flake without buying anything.

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
import federation from './check/federation.mjs';
import gossip from './check/gossip.mjs';

// The first five run against mocks or in-memory servers; e2e, federation and
// gossip drive the real Go binary (so `go` must be on PATH).
const CHECKS = [layout, shapes, resilience, sse, a11y, e2e, federation, gossip];

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
  console.log(`${mark} ${mod.name.padEnd(12)} ${String(c.passed).padStart(3)} passed  ${String(c.failed).padStart(3)} failed  (${ms}ms)`);
  for (const f of c.results.filter((r) => !r.pass)) console.log(`     \u2717 ${f.label}`);
}

await browser.close();
close();
await writeFile(join(OUT, 'report.json'), JSON.stringify(report, null, 2));

console.log('\n' + '\u2500'.repeat(56));
const byKind = report.issues.reduce((a, i) => ((a[i.kind] = (a[i.kind] ?? 0) + 1), a), {});
if (report.issues.length) {
  console.log('Issue breakdown:');
  for (const [k, n] of Object.entries(byKind).sort((a, b) => b[1] - a[1])) console.log(`  ${k.padEnd(18)} ${n}`);
  console.log('');
}
for (const i of report.issues.slice(0, 25)) {
  if (i.kind === 'clipped') console.log(`  [clipped] ${i.vp}/${i.tab} ${i.sel} "${i.text}" left=${i.left} right=${i.right}`);
  else if (i.kind === 'flush-edge') console.log(`  [flush] ${i.vp}/${i.tab} ${i.sel} "${i.text}" gapL=${i.gapL}px gapR=${i.gapR}px`);
  else if (i.kind === 'small-target') console.log(`  [small target] ${i.vp}/${i.tab} ${i.sel} "${i.text}" ${i.w}\u00d7${i.h}`);
  else if (i.kind === 'a11y') console.log(`  [a11y] ${i.id} (${i.impact}) ${i.help} @ ${i.nodes?.join(', ')}`);
  else console.log(`  [${i.kind}] ${i.check} ${i.label ?? i.message ?? ''}`);
}
if (report.issues.length > 25) console.log(`  ... ${report.issues.length - 25} more, see report.json`);

console.log(`\nTotal ${totalPass} passed / ${totalFail} failed`);
process.exit(totalFail === 0 ? 0 : 1);
