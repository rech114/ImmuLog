/* SPDX-License-Identifier: Apache-2.0 */
// mock.js —— 只被 stream.js 引用（?demo=1）。
// 目的：后端还没写，但界面要先能看。契约与真实 SSE 完全相同。

const HEX = 'abcdef0123456789';
const oid = () => Array.from({ length: 40 }, () => HEX[(Math.random() * 16) | 0]).join('');

const GENESIS = oid();
const ALICE = oid();
const BOB = oid();
const CAROL = oid();

const script = (send) => [
  [0, { type: 'hello', head: CAROL, anchoredAt: '今天 08:00',
    // 演示模式没有真的签名密钥 —— 如实标为未签名，不粉饰
    identity: { signed: false },
    peers: [
      { name: 'origin', url: 'git@node-a', ok: true },
      { name: 'mirror', url: 'git@node-b', ok: true },
      { name: 'alice 的机器', url: 'http://10.0.0.7:8081', ok: false, note: '与本机见证锚不一致' },
    ] }],

  [300, { type: 'msg', oid: GENESIS, seq: 1, author: 'alice', body: '建好了，这条是创世提交。' }],

  [900, { type: 'msg', oid: ALICE, seq: 2, author: 'alice',
    body: '我把 GitChat 那套搬过来了，但写路径换成了 commit-tree。', sig: 'ssh-ed25519 …9f2a ✓' }],

  [1500, { type: 'msg', oid: BOB, seq: 3, author: 'bob',
    body: '所以服务端现在连 index 都不需要了？', sig: 'ssh-ed25519 …4c71 ✓' }],

  [2200, { type: 'msg', oid: CAROL, seq: 4, author: 'carol',
    body: '对，而且 ref 更新走 CAS，被抢先就直接失败，不会静默丢消息。', sig: 'ssh-ed25519 …ab03 ✓' }],

  [3200, { type: 'retract', oid: oid(), retracts: ALICE, reason: '说错了一个细节' }],

  [4600, { type: 'alarm', oid: oid(), title: '检测到历史改写',
    detail: '服务端提供的 alice feed 与本机见证不一致，且没有重写公告。本地副本已保留，拒绝覆盖。',
    local: '3f2a1b…', remote: '9c8b4d…' }],

  [5600, { type: 'msg', oid: oid(), seq: 5, author: 'carol',
    body: '看，这就是我们要的：它能改，但改不静默。', sig: 'ssh-ed25519 …ab03 ✓' }],
];

export function mock({ onEvent, onLink }) {
  onLink?.('up');
  const timers = script(onEvent).map(([delay, evt]) => setTimeout(() => onEvent(evt), delay));

  // 每 9 秒模拟一条新消息，确认界面是活的
  let seq = 6;
  const beat = setInterval(() => {
    onEvent({ type: 'msg', oid: oid(), seq: seq++, author: 'bob',
      body: `心跳 ${seq}` , sig: 'ssh-ed25519 …4c71 ✓' });
  }, 9000);

  return () => { timers.forEach(clearTimeout); clearInterval(beat); };
}
