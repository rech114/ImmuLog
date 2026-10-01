import { readFile } from 'node:fs/promises';
import { JSDOM } from 'jsdom';

const html = await readFile('../web/index.html', 'utf8');
const css = await readFile('../web/style.css', 'utf8');

let depth = 0, line = 1, bad = [];
for (const ch of css) { if (ch === '\n') line++; if (ch === '{') depth++; if (ch === '}') { depth--; if (depth < 0) bad.push(line); } }
console.log(`花括号平衡: ${depth === 0 && !bad.length ? '\u2713' : '\u2717 深度=' + depth + ' 异常行=' + bad}`);

const dom = new JSDOM(html, { url: 'http://x/' });
const style = dom.window.document.createElement('style');
style.textContent = css;
dom.window.document.head.appendChild(style);

const sheet = dom.window.document.styleSheets[0];
console.log(`解析出规则数: ${sheet.cssRules.length}`);
const sels = [...sheet.cssRules].map(r => r.selectorText).filter(Boolean);
for (const want of ['.kv', '.kv > i', '.kv strong', '.kv p', 'p.no-margin', '.msg .text', '.alarm p']) {
  console.log(`  ${want.padEnd(16)} ${sels.includes(want) ? '\u2713' : '\u2717 丢失'}`);
}
console.log('\nHTML 里 .kv 的结构:');
const kvs = dom.window.document.querySelectorAll('.kv');
console.log(`  .kv 元素数: ${kvs.length}`);
if (kvs[0]) {
  console.log('  第一个 .kv 的子元素:');
  for (const c of kvs[0].children) console.log(`    <${c.tagName.toLowerCase()} class="${c.className}">`);
  const inner = kvs[0].querySelector('.max');
  if (inner) for (const c of inner.children) console.log(`    .max > <${c.tagName.toLowerCase()} class="${c.className}">`);
}
console.log('\n计算样式（jsdom 级）:');
if (kvs[0]) {
  const cs = dom.window.getComputedStyle(kvs[0]);
  console.log(`  .kv display = ${cs.display}`);
}
