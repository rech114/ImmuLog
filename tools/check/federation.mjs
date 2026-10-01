// check/federation.mjs —— 双节点端到端：真正的多源同步与分裂视图检测。
//
// 上游所有检查都是单节点。只有这一条起两个真实进程、共用一个中转仓库，
// 验证「阶段四」承诺的三件事：
//   · 别人的消息能同步过来，并落成可信状态
//   · 中转站被改写时，本机拒绝覆盖并留下告警
//   · 快照能被独立复算

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

export default async function federation(browser, _base, c) {
  const work = await mkdtemp(join(tmpdir(), 'immutalk-fed-'));
  const bin = join(work, 'immutalk');
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

    const body = '联邦冒烟 · ' + Math.random().toString(36).slice(2, 8);
    const sent = await (await fetch(A.base + '/api/commit', {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ kind: 'msg', body }),
    })).json();
    c.ok(/^[0-9a-f]{40}$/.test(sent.oid || ''), `A 发出消息（${String(sent.oid).slice(0, 8)}…）`);
    c.ok(sent.seq === 1, `A 的序号是 1（实际 ${sent.seq}）`);

    // 保证中转站拿到 A 的 feed（本机 receive-pack 不可用，详见 README）
    publish(hub, aRepo);
    const hubRef = git(hub, 'for-each-ref', '--format=%(refname)', 'refs/feeds/');
    c.ok(hubRef === `refs/feeds/${healthA.feed}`, `中转站持有 A 的 feed（${hubRef}）`);

    // ── 3) 节点 B：同步过来 ─────────────────────────────────────
    B = startNode({
      bin, repo: bRepo, port: portB, remotes: [remote], who: 'bob',
      env: { IMMUTALK_SYNC_INTERVAL: SYNC_MS, IMMUTALK_ANCHOR_INTERVAL: '1s' },
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
    const first = git(aRepo, 'rev-list', '--max-parents=0', aRef);
    const tree = git(aRepo, 'hash-object', '-w', '-t', 'tree', '--stdin');
    // 从根提交另起一条平行链
    const forged = gitIn(aRepo, '被改写的历史\n\nImmutalk-Kind: msg\nImmutalk-Seq: 1\n',
      'commit-tree', tree);
    c.ok(forged !== sent.oid, '前置条件：伪造的是另一个对象');

    git(hub, 'update-ref', aRef, forged); // 中转站被改写
    c.ok(git(hub, 'rev-parse', aRef) === forged, '中转站已指向被改写的历史');

    // 再发一条无关消息，逼 B 在下一轮同步里撞上矛盾
    await fetch(A.base + '/api/commit', {
      method: 'POST', headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ kind: 'msg', body: '第二条' }),
    });
    publish(hub, aRepo);

    await page.waitForSelector('.alarm', { timeout: 25000 });
    const alarm = await page.evaluate(() => ({
      title: document.querySelector('.alarm h6')?.textContent ?? '',
      shape: document.querySelector('.alarm .shape')?.className ?? '',
    }));
    c.ok(alarm.title.includes('改写'), `B 报出改写告警：「${alarm.title}」`);
    c.ok(alarm.shape.includes('burst'), '告警形状为 burst');

    // 核心：B 的可信状态与见证锚不得被带动
    c.ok(git(bRepo, 'rev-parse', aRef) === bTip, 'B 的本地副本拒绝被覆盖（保留原样）');
    c.ok(git(bRepo, 'rev-parse', `refs/witness/${healthA.feed}`) === bWitness, 'B 的见证锚纹丝不动');

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
