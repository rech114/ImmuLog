// SPDX-License-Identifier: Apache-2.0
// check/federation.mjs —— 双节点端到端：真正的多源同步与分裂视图检测。
//
// 上游所有检查都是单节点。只有这一条起两个真实进程、共用一个中转仓库，
// 验证「阶段四」承诺的三件事：
//   · 别人的消息能同步过来，并落成可信状态
//   · 中转站被改写时，本机拒绝覆盖并留下告警
//   · 快照能被独立复算

import { execFileSync } from 'node:child_process';
import { mkdtemp, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { VIEWPORTS, session, shot } from '../lib/harness.mjs';
import {
  GIT_ENV, git, gitIn, freePort, buildBinary, initRepo, startNode,
  waitHealthy, stopNode, publish,
} from '../lib/harness.mjs';

const SYNC_MS = '400ms';

/** 等页面里出现某段文字。 */
async function waitForText(page, text, timeout = 20000) {
  await page.waitForFunction(
    (t) => [...document.querySelectorAll('.msg .text')].some((e) => e.textContent.includes(t)),
    text, { timeout },
  );
}

/** 判断 a 是否为 b 的祖先；git 用非零退出表示"否"，不是错误。 */
function isAncestor(dir, a, b) {
  try {
    execFileSync('git', ['-C', dir, 'merge-base', '--is-ancestor', a, b], { env: GIT_ENV, stdio: 'ignore' });
    return true;
  } catch {
    return false;
  }
}

export default async function federation(browser, _base, c) {
  const work = await mkdtemp(join(tmpdir(), 'immulog-fed-'));
  const bin = join(work, 'immulog');
  const hub = join(work, 'hub.git');
  const aRepo = join(work, 'repoA');
  const bRepo = join(work, 'repoB');
  let A, B;

  try {
    // ── 1) 构建 + 造场景 ────────────────────────────────────────
    try {
      buildBinary(bin);
      c.ok(true, 'go build 成功');
    } catch (e) {
      c.ok(false, `go build 失败：${String(e.stderr || e).slice(0, 300)}`);
      return;
    }
    initRepo(hub, 'hub');
    initRepo(aRepo, 'alice');
    initRepo(bRepo, 'bob');

    const remote = `hub=${hub}`;
    const portA = await freePort();
    const portB = await freePort();

    // ── 2) 节点 A：发一条消息 ───────────────────────────────────
    A = startNode({ bin, repo: aRepo, port: portA, remotes: [remote], who: 'alice' });
    const healthA = await waitHealthy(A.base).catch((e) => {
      c.ok(false, `${e.message}\nA 日志：\n${A.log.slice(-600)}`);
      return null;
    });
    if (!healthA) return;

    const post = async (base, body) => (await fetch(base + '/api/commit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ kind: 'msg', body }),
    })).json();

    // 发两条：链上必须有一个"分叉点"，否则从链尾分叉出来的只是正常追加，
    // 根本构不成"引用重写"。
    const body = '联邦冒烟 · ' + Math.random().toString(36).slice(2, 8);
    const first = await post(A.base, body);
    c.ok(/^[0-9a-f]{40}$/.test(first.oid || ''), `A 发出第一条（${String(first.oid).slice(0, 8)}…）`);
    c.ok(first.seq === 1, `A 的序号从 1 开始（实际 ${first.seq}）`);

    const sent = await post(A.base, '第二条：给链留一个分叉点');
    c.ok(sent.seq === 2, `A 的序号递增到 2（实际 ${sent.seq}）`);

    // 保证中转站拿到 A 的 feed（本机 receive-pack 不可用，详见 README）
    publish(hub, aRepo);
    const hubRef = git(hub, 'for-each-ref', '--format=%(refname)', 'refs/feeds/');
    c.ok(hubRef === `refs/feeds/${healthA.feed}`, `中转站持有 A 的 feed（${hubRef}）`);

    // ── 3) 节点 B：同步过来 ─────────────────────────────────────
    B = startNode({
      bin, repo: bRepo, port: portB, remotes: [remote], who: 'bob',
      env: { IMMULOG_SYNC_INTERVAL: SYNC_MS, IMMULOG_ANCHOR_INTERVAL: '1s' },
    });
    const healthB = await waitHealthy(B.base).catch((e) => {
      c.ok(false, `${e.message}\nB 日志：\n${B.log.slice(-600)}`);
      return null;
    });
    if (!healthB) return;
    c.ok(healthB.feed !== healthA.feed, '两个节点是不同的身份');

    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    const errors = [];
    page.on('pageerror', (e) => errors.push(String(e).slice(0, 200)));
    await page.goto(B.base + '/', { waitUntil: 'load' });
    await page.waitForSelector('#input', { timeout: 15000 });

    await waitForText(page, body);
    c.ok(true, 'B 通过同步收到了 A 的消息');

    const got = await page.evaluate((t) => {
      const el = [...document.querySelectorAll('.msg')].find((e) => e.textContent.includes(t));
      return { state: el?.dataset.state, author: el?.querySelector('.who')?.textContent };
    }, body);
    c.ok(got.state === 'verified', `同步来的消息状态为 verified（${got.state}）`);
    c.ok(got.author === 'alice', `作者取自提交（${got.author}）`);

    // B 侧确实把它落成了可信状态，而不只是 UI 上显示
    const aRef = `refs/feeds/${healthA.feed}`;
    const bTip = git(bRepo, 'rev-parse', aRef);
    c.ok(bTip === sent.oid, 'B 把外来 feed 落进了 refs/feeds/*');
    const bWitness = git(bRepo, 'rev-parse', `refs/witness/${healthA.feed}`);
    c.ok(bWitness === sent.oid, 'B 为外来 feed 建立了见证锚');

    // ── 4) 快照与对端状况 ───────────────────────────────────────
    const snap = await (await fetch(B.base + '/api/snapshot')).json();
    c.ok(/^[0-9a-f]{40}$/.test(snap.digest || ''), `B 能独立算出快照摘要（${String(snap.digest).slice(0, 8)}…）`);
    c.ok((snap.refs || []).some((r) => r.name === aRef), '快照里包含 A 的 feed');
    c.ok((snap.peers || []).length === 1 && snap.peers[0].ok, '对端状况为可达且一致');

    // 锚定链
    await page.waitForTimeout(1500);
    const anchored = git(bRepo, 'for-each-ref', '--format=%(refname)', 'refs/anchors/');
    c.ok(anchored.includes('refs/anchors/'), `B 产生了锚定（${anchored.split('\n')[0]}）`);

    // ── 5) 攻击者改写中转站上的 A feed ──────────────────────────
    //
    // 从**第一条**分叉，得到第二条的兄弟 —— 这样伪造的链尾与本机链尾
    // 互不构成祖先关系，才是真正的"引用重写"。
    //
    // 在克隆里造链再发布：伪造的提交必须是**可达的**，否则 `refs/feeds/*`
    // 的传输带不走它（hub 是另一个仓库，拿不到游离对象）。
    const liarDir = join(work, 'liar.git');
    git(work, 'clone', '--bare', '--quiet', aRepo, liarDir);
    git(liarDir, 'config', 'user.name', 'alice');
    git(liarDir, 'config', 'user.email', 'alice@example.com');

    const tree = git(liarDir, 'hash-object', '-w', '-t', 'tree', '--stdin');
    const forged = gitIn(liarDir, '被改写的历史\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n',
      'commit-tree', tree, '-p', first.oid);
    c.ok(forged !== sent.oid, '前置条件：伪造的是另一个对象');
    git(liarDir, 'update-ref', aRef, forged);
    publish(hub, liarDir); // 中转站被改写
    c.ok(git(hub, 'rev-parse', aRef) === forged, '中转站已指向被改写的历史');

    // 不再重新发布诚实链 —— 否则中转站立刻被覆盖回诚实版本，B 根本撞不上矛盾
    await page.waitForSelector('.alarm', { timeout: 25000 });
    const alarm = await page.evaluate(() => ({
      title: document.querySelector('.alarm h6')?.textContent ?? '',
      shape: document.querySelector('.alarm .shape')?.className ?? '',
    }));
    c.ok(alarm.title.includes('改写'), `B 报出改写告警：「${alarm.title}」`);
    c.ok(alarm.shape.includes('burst'), '告警形状为 burst');

    // ── 6) 核心判据：本机状态必须完好无损 ───────────────────────
    const afterTip = git(bRepo, 'rev-parse', aRef);
    const afterWitness = git(bRepo, 'rev-parse', `refs/witness/${healthA.feed}`);
    c.ok(afterTip === sent.oid, 'B 的本地副本拒绝被覆盖（仍是诚实链尾）');
    c.ok(afterWitness === sent.oid, 'B 的见证锚纹丝不动');

    c.ok(!isAncestor(bRepo, forged, afterTip), '伪造的提交不在 B 的历史里');
    c.ok(isAncestor(bRepo, sent.oid, afterTip), '诚实的完整历史仍在 B 手里');

    const peers = (await (await fetch(B.base + '/api/snapshot')).json()).peers ?? [];
    c.ok(peers.length === 1 && peers[0].ok === false, '对端被标为不一致');

    await shot(page, 'federation-alarm.png');

    const real = errors.filter((e) => !/favicon|404/i.test(e));
    c.ok(real.length === 0, '全程无未捕获 JS 错误', { errors: real });

    await ctx.close();
  } finally {
    stopNode(A);
    stopNode(B);
    await rm(work, { recursive: true, force: true });
  }
}
