// tools/smoke.mjs —— 前端冒烟测试（开发工具，不进 embed）
//
// 两个场景，都是黑盒：只通过 DOM 事件驱动，不 import 内部符号。
//   A  ?demo=1   模拟事件流 → 渲染链路
//   B  无 demo   真实 wiring：EventSource 帧 + fetch 失败路径（cas_failed）
//
// 每个场景把 web/app 拷到独立临时目录再 import，避免 ESM 模块缓存串场。

import { readFile, cp, rm } from 'node:fs/promises';
import { join, dirname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';
import { JSDOM } from 'jsdom';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..');
const APP = join(ROOT, 'web', 'app');
const HTML = await readFile(join(ROOT, 'web', 'index.html'), 'utf8');

let passed = 0, failed = 0;
const ok = (cond, label) => {
  if (cond) { passed++; console.log(`   \u2713 ${label}`); }
  else { failed++; console.log(`   \u2717 ${label}`); }
};
const flush = () => new Promise((r) => setImmediate(r));
const headline = (s) => console.log(`\n${s}`);

// ── 确定性时钟（接管 setTimeout/setInterval，让 mock 时间线可快进）─────

let seq = 0, now = 0;
const timers = new Map();
globalThis.setTimeout = (fn, ms = 0, ...args) => { const id = ++seq; timers.set(id, { fn, at: now + ms, args, every: 0 }); return id; };
globalThis.setInterval = (fn, ms = 0, ...args) => { const id = ++seq; timers.set(id, { fn, at: now + ms, args, every: ms || 1 }); return id; };
globalThis.clearTimeout = (id) => timers.delete(id);
globalThis.clearInterval = (id) => timers.delete(id);

function advance(ms) {
  const end = now + ms;
  for (;;) {
    let pick = null;
    for (const [id, t] of timers) if (t.at <= end && (!pick || t.at < pick[1].at)) pick = [id, t];
    if (!pick) break;
    const [id, t] = pick;
    now = t.at;
    if (t.every) t.at = now + t.every; else timers.delete(id);
    t.fn(...t.args);
  }
  now = end;
}

// ── 隔离装载 ──────────────────────────────────────────────────────

async function boot(tag, search) {
  timers.clear(); seq = 0; now = 0;

  const dom = new JSDOM(HTML, {
    url: `http://127.0.0.1:8099/${search}`,
    pretendToBeVisual: true,
  });

  globalThis.window = dom.window;
  globalThis.document = dom.window.document;
  globalThis.location = dom.window.location;
  globalThis.requestAnimationFrame = dom.window.requestAnimationFrame.bind(dom.window);
  dom.window.scrollTo = () => { };                   // jsdom 不实现
  dom.window.uiCalls = 0;
  globalThis.ui = () => { dom.window.uiCalls++; };   // Beer CSS 的 JS：桩

  const dir = `/tmp/immutalk-smoke-${tag}`;
  await rm(dir, { recursive: true, force: true });
  await cp(APP, dir, { recursive: true });
  await import(pathToFileURL(join(dir, 'main.js')).href);

  return dom;
}

// ── 场景 A：模拟事件流 → 渲染 ─────────────────────────────────────

headline('场景 A · ?demo=1 模拟时间线 → 渲染');
{
  const dom = await boot('a', '?demo=1');
  const $ = (s) => dom.window.document.querySelectorAll(s);
  const one = (s) => dom.window.document.querySelector(s);

  ok($('.msg').length === 0, '初始渲染为空');

  advance(300); await flush();
  ok($('.msg').length === 1, '创世消息上屏');
  ok(one('#room-id').textContent !== '——', `房间身份 = 创世 OID 前 6 位（${one('#room-id').textContent}）`);

  advance(600); await flush();
  ok($('.msg').length === 2, '第二条消息追加');

  advance(600); await flush();
  const bob = $('.msg')[2];
  ok(bob && bob.dataset.state === 'verified', 'bob 的消息为已验签');
  ok(bob && bob.querySelector('.shape').className.includes('gem'), '已验签 → 形状 gem');

  advance(700); await flush();
  ok($('.msg').length === 4, '第四条消息追加');

  // 撤回
  const alice = $('.msg')[1];
  ok(alice.dataset.state === 'verified', '撤回前：alice 消息为 verified');
  advance(1000); await flush();
  ok(alice.dataset.state === 'retracted', '撤回后：原条目被标记 retracted（未删除）');
  ok(alice.querySelector('.shape').className.includes('slanted'), '撤回 → 形状切到 slanted');
  ok(alice.querySelector('.strike-note').textContent.includes('已撤回'), '留痕文案出现');
  ok($('.msg').length === 4, '消息总数不变（撤回是追加事件，不是删除）');

  // 篡改告警
  advance(1400); await flush();
  ok($('.alarm').length === 1, '告警作成封条插入时间线');
  ok(one('.alarm .shape').className.includes('burst'), '告警 → 形状 burst');
  ok(one('#alarm-log .kv') !== null, '告警同时进入完整性页签的告警记录');
  ok(one('#alarm-log .placeholder') === null, '告警记录里的「无」占位符被清除（不残留）');

  // 心跳
  advance(9000); await flush();
  ok($('.msg').length === 6, '心跳消息到达（时间线是活的）');

  // 视图切换
  one('#views button[data-view="integrity"]').click();
  ok(one('.view[data-view="integrity"]').classList.contains('active'), '切到「完整性」页签');
  ok(!one('.view[data-view="chat"]').classList.contains('active'), '「消息」页签已失活');
  ok(one('#anchor-count').textContent !== '0', `见证锚计数已更新（${one('#anchor-count').textContent}）`);

  // 主题
  const before = dom.window.document.body.classList.contains('dark');
  one('#theme').click();
  ok(dom.window.document.body.classList.contains('dark') !== before, '明暗主题可切换');
  ok(dom.window.uiCalls > 0, '动态主题色已下发给 Beer CSS');

  // 乐观投递
  const dupBefore = $('.msg').length;
  one('#input').value = '冒烟测试';
  one('#send').click();
  await flush();
  const mine = [...$('.msg')].at(-1);
  ok(mine.querySelector('.text').textContent.includes('冒烟测试'), '乐观投递：消息先上屏');
  ok($('.msg').length === dupBefore + 1, '只多一条');
  const pendingShape = mine.querySelector('.shape').className;
  ok(pendingShape.includes('loading-indicator'), '未落地 → 形状 loading-indicator');
  ok(mine.dataset.state === 'pending', '未落地 → 状态 pending');
  ok(mine.dataset.oid.startsWith('local-'), '未落地 → 还是临时 OID');

  advance(500); await flush();
  ok(mine.dataset.state === 'verified', '落地后原地转 verified');
  ok(mine.dataset.oid.length === 40, '临时 OID 已换成真实 40 位哈希');
  ok(mine.querySelector('.shape').className.includes('gem'), '落地后形状 → gem（原地替换，气泡不跳）');
  ok(one('#input').value === '', '输入框已清空');
}

// ── 场景 B：真实 wiring（SSE 帧 + CAS 失败）───────────────────────

headline('场景 B · 无 demo：EventSource 帧 + fetch 失败路径');
{
  class FakeES {
    constructor(url) { this.url = url; this.listeners = {}; FakeES.last = this; }
    addEventListener(t, fn) { (this.listeners[t] ||= []).push(fn); }
    close() { this.closed = true; }
    emit(type, payload, id) {
      for (const fn of this.listeners[type] || []) fn({ type, data: JSON.stringify(payload), lastEventId: id });
    }
  }
  const fetchCalls = [];
  globalThis.EventSource = FakeES;
  globalThis.fetch = async (url, opts) => {
    fetchCalls.push({ url, body: JSON.parse(opts.body) });
    return { ok: false, status: 409, json: async () => ({ error: 'cas_failed', expected: 'aaa', actual: 'bbb' }) };
  };

  const dom = await boot('b', '');
  const $ = (s) => dom.window.document.querySelectorAll(s);
  const one = (s) => dom.window.document.querySelector(s);
  const OID = 'a'.repeat(40);

  ok(FakeES.last !== undefined, '已建立 SSE 连接');
  ok(FakeES.last.url === '/api/stream', '连到 /api/stream');
  ok($('.msg').length === 0, '初始为空（不伪造首屏）');

  FakeES.last.emit('msg', { oid: OID, seq: 1, author: 'alice', body: '来自真实流' }, OID);
  await flush();
  ok($('.msg').length === 1, 'SSE 帧渲染成消息');
  ok(one('.msg .text').textContent.includes('来自真实流'), '正文正确');

  FakeES.last.emit('msg', { oid: OID, seq: 1, author: 'alice', body: '来自真实流' }, OID);
  await flush();
  ok($('.msg').length === 1, '相同 OID 去重，不重复渲染');

  FakeES.last.emit('retract', { oid: 'b'.repeat(40), retracts: OID, reason: '说错了' }, 'b'.repeat(40));
  await flush();
  ok(one('.msg').dataset.state === 'retracted', 'SSE 撤回帧生效');
  ok(one('.msg .strike-note').textContent.includes('说错了'), '撤回原因上屏');

  FakeES.last.emit('alarm', { oid: 'c'.repeat(40), title: '检测到历史改写', local: '111111', remote: '222222' }, 'c'.repeat(40));
  await flush();
  ok($('.alarm').length === 1, 'SSE 告警帧生效');

  FakeES.last.emit('hello', { head: OID, anchoredAt: '今天 08:00', peers: [{ name: 'mirror', url: 'git@node-b', ok: true }] }, '');
  await flush();
  ok(one('#anchored-at').textContent.includes('08:00'), 'hello 帧更新外部锚定时间');
  ok(one('#peers .kv') !== null, 'hello 帧渲染对端列表');

  // 发送：服务端返回 cas_failed
  one('#input').value = '这条会被拒';
  one('#send').click();
  await flush(); await flush();

  ok(fetchCalls.length === 1, '写入口只发一次请求');
  ok(fetchCalls[0].url === '/api/commit', 'POST 到 /api/commit');
  ok(fetchCalls[0].body.kind === 'msg', 'kind 区分事件类型');
  ok(one('.msg[data-state="unverified"]') !== null, 'cas_failed → 消息标记为未验签');
  ok(one('#toast').classList.contains('active'), 'cas_failed 未被吞掉，已提示用户');
  ok(one('#toast-text').textContent.includes('cas_failed'), `提示文案含错误码（${one('#toast-text').textContent}）`);
}

// ── 结果 ──────────────────────────────────────────────────────────

console.log(`\n${'─'.repeat(52)}`);
console.log(failed === 0 ? `\u2705 全量冒烟通过  ${passed} 项` : `\u274c ${failed} 项失败 / ${passed} 项通过`);
process.exit(failed === 0 ? 0 : 1);
