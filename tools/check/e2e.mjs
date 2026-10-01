// SPDX-License-Identifier: Apache-2.0
// check/e2e.mjs —— 端到端：真实 Go 二进制 + 真实浏览器 + 真实 git 仓库。
//
// 上游所有检查都跑在 mock 或内存服务上。只有这一条把三样东西接在一起，
// 所以它是唯一能证明「前端 + 后端 + 原版 Git」真的合得上的检查。
//
// 覆盖：
//   · 服务能起来、前端连得上、SSE 首屏回放
//   · 从 UI 发消息 → 落成真实的 git commit（用 git CLI 反查）
//   · 刷新后仍在（真的持久化了，不是前端内存）
//   · 撤回是追加事件，原消息仍在链上
//   · 外部 force-push → 页面出现告警封条

import { spawn, execFileSync } from 'node:child_process';
import { mkdtemp, rm, mkdir } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { chromium } from 'playwright';
import { VIEWPORTS, session, collector, shot, ROOT } from '../lib/harness.mjs';

const GIT_ENV = {
  ...process.env,
  GIT_CONFIG_GLOBAL: '/dev/null',
  GIT_CONFIG_SYSTEM: '/dev/null',
  GIT_AUTHOR_NAME: 'alice',
  GIT_AUTHOR_EMAIL: 'alice@example.com',
  GIT_COMMITTER_NAME: 'alice',
  GIT_COMMITTER_EMAIL: 'alice@example.com',
};

const git = (dir, ...args) =>
  execFileSync('git', ['-C', dir, ...args], { env: GIT_ENV, encoding: 'utf8' }).trim();

async function freePort() {
  const { createServer } = await import('node:net');
  return new Promise((resolve, reject) => {
    const s = createServer();
    s.on('error', reject);
    s.listen(0, '127.0.0.1', () => {
      const { port } = s.address();
      s.close(() => resolve(port));
    });
  });
}

async function waitHealthy(base, ms = 30000) {
  const deadline = Date.now() + ms;
  let lastErr;
  while (Date.now() < deadline) {
    try {
      const r = await fetch(base + '/api/health');
      if (r.ok) return await r.json();
    } catch (e) {
      lastErr = e;
    }
    await new Promise((r) => setTimeout(r, 250));
  }
  throw new Error(`服务未在 ${ms}ms 内就绪：${lastErr}`);
}

export default async function e2e(browser, _base, c) {
  const work = await mkdtemp(join(tmpdir(), 'immulog-e2e-'));
  const repo = join(work, 'repoDB');
  const bin = join(work, 'immulog');
  const port = await freePort();
  const base = `http://127.0.0.1:${port}`;
  let proc;

  try {
    // ── 1) 构建真实二进制 ────────────────────────────────────────
    try {
      execFileSync('go', ['build', '-o', bin, '.'], { cwd: ROOT, stdio: 'pipe' });
      c.ok(true, 'go build 成功');
    } catch (e) {
      c.ok(false, `go build 失败：${String(e.stderr || e).slice(0, 400)}`);
      return;
    }

    // ── 2) 造一个 bare 仓库并配身份（服务端只读 git 配置，不吃环境变量）──
    await mkdir(repo, { recursive: true });
    git(repo, 'init', '--bare', '--quiet', '-b', 'main');
    git(repo, 'config', 'user.name', 'alice');
    git(repo, 'config', 'user.email', 'alice@example.com');

    // ── 3) 起服务 ───────────────────────────────────────────────
    proc = spawn(bin, [], {
      env: { ...GIT_ENV, IMMULOG_REPO: repo, PORT: String(port) },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let serverLog = '';
    proc.stdout.on('data', (d) => (serverLog += d));
    proc.stderr.on('data', (d) => (serverLog += d));

    const health = await waitHealthy(base).catch((e) => {
      c.ok(false, `${e.message}\n服务端日志：\n${serverLog.slice(-800)}`);
      return null;
    });
    if (!health) return;
    c.ok(health.ok === true, `服务就绪，feed=${health.feed} signed=${health.signed}`);
    c.ok(health.signed === false, '未配置签名密钥时明确报告 signed=false（不假装安全）');

    // ── 4) 真实浏览器打开真实后端（注意：没有 ?demo=1）───────────
    const { ctx, page } = await session(browser, VIEWPORTS[0]);
    const consoleErrors = [];
    page.on('console', (m) => m.type() === 'error' && consoleErrors.push(m.text().slice(0, 200)));
    page.on('pageerror', (e) => consoleErrors.push(String(e).slice(0, 200)));

    await page.goto(base + '/', { waitUntil: 'load' });
    await page.waitForSelector('#input', { timeout: 15000 });
    await page.waitForTimeout(1200);

    // 空 feed 应有空态，而不是白板
    const empty = await page.evaluate(() => ({
      emptyShown: !!document.querySelector('#chat-empty'),
      msgs: document.querySelectorAll('.msg').length,
    }));
    c.ok(empty.msgs === 0, '初始没有消息');
    c.ok(empty.emptyShown, '空 feed 显示空态（不是白板）');

    // ── 5) 从 UI 发消息 ─────────────────────────────────────────
    const body = '端到端冒烟 · ' + Math.random().toString(36).slice(2, 8);
    await page.fill('#input', body);
    await page.click('#send');

    // 关键：乐观投递的临时气泡**立刻**就有文字，所以不能只等文字。
    // 必须等到落地（非 pending 且 OID 是真实的 40 位对象名）。
    await page.waitForFunction(
      (t) => {
        const el = [...document.querySelectorAll('.msg')].find((e) => e.textContent.includes(t));
        return !!el && el.dataset.state !== 'pending' && /^[0-9a-f]{40}$/.test(el.dataset.oid);
      },
      body, { timeout: 15000 },
    );
    const afterSend = await page.evaluate((t) => {
      const el = [...document.querySelectorAll('.msg')].find((e) => e.textContent.includes(t));
      return { state: el.dataset.state, oid: el.dataset.oid, shape: el.querySelector('.shape').className };
    }, body);
    c.ok(afterSend.state === 'verified', `发送后状态为 verified（实际 ${afterSend.state}）`);
    c.ok(afterSend.shape.includes('gem'), '已验签 → 形状 gem');
    c.ok(/^[0-9a-f]{40}$/.test(afterSend.oid), `DOM 上的 OID 是真实 40 位对象名（${afterSend.oid.slice(0, 8)}…）`);

    // ── 6) 反查 git：它必须是一个真实的 commit 对象 ─────────────
    const ref = git(repo, 'for-each-ref', '--format=%(refname)', 'refs/feeds/');
    c.ok(ref === `refs/feeds/${health.feed}`, `消息落在 refs/feeds/<feed>（${ref}）`);

    const logged = git(repo, 'log', '--format=%H%x1f%an%x1f%(trailers:key=ImmuLog-Seq,valueonly)', ref);
    const [oid, author, seq] = logged.split('\x1f');
    c.ok(oid === afterSend.oid, 'DOM 上的 OID 与 git 里的对象一致');
    c.ok(author === 'alice', `作者取自 git 配置（${author}）`);
    c.ok(seq.trim() === '1', `ImmuLog-Seq trailer 写入正确（${JSON.stringify(seq)}）`);

    // bare 仓库里没有 index / worktree —— 证明写路径没用 `git commit`
    c.ok(git(repo, 'rev-parse', '--is-bare-repository') === 'true', '仓库是 bare（无 worktree、无 index）');

    // ── 7) 刷新后仍在（真的持久化）──────────────────────────────
    await page.reload({ waitUntil: 'load' });
    await page.waitForSelector('.msg', { timeout: 15000 });
    const persisted = await page.evaluate(
      (t) => [...document.querySelectorAll('.msg .text')].some((e) => e.textContent.includes(t)), body);
    c.ok(persisted, '刷新后消息仍在（SSE 首屏回放走的是真实历史）');

    // 乐观投递的去重：最终 DOM 里这条消息只应有一份
    const dupes = await page.evaluate(
      (t) => [...document.querySelectorAll('.msg')].filter((e) => e.textContent.includes(t)).length, body);
    c.ok(dupes === 1, `乐观投递与 SSE 广播按 OID 去重，DOM 里只有一份（实际 ${dupes}）`);

    // ── 8) 撤回是追加事件 ───────────────────────────────────────
    const retractResp = await page.evaluate(async (target) => {
      const r = await fetch('/api/commit', {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ kind: 'retract', retracts: target, reason: '端到端撤回' }),
      });
      return { status: r.status, body: await r.json() };
    }, afterSend.oid);
    c.ok(retractResp.status === 201, `撤回接口返回 201（实际 ${retractResp.status}）`);

    await page.waitForFunction(
      (o) => document.querySelector(`.msg[data-oid="${o}"]`)?.dataset.state === 'retracted',
      afterSend.oid, { timeout: 10000 },
    );
    const n = git(repo, 'rev-list', '--count', ref);
    c.ok(n === '2', `撤回后链长为 2 —— 撤回是追加，不是删除（${n}）`);
    const still = git(repo, 'log', '--format=%s', ref);
    c.ok(still.includes(body), '被撤回的原消息仍在链上（永不删除）');

    // ── 9) 外部 force-push → 页面必须出现告警封条 ───────────────
    const parent = git(repo, 'rev-parse', afterSend.oid);
    const tree = git(repo, 'hash-object', '-w', '-t', 'tree', '--stdin');
    const forged = execFileSync(
      'git',
      ['-C', repo, 'commit-tree', tree, '-p', parent],
      { env: GIT_ENV, input: '被改写的历史\n\nImmuLog-Kind: msg\nImmuLog-Seq: 2\n', encoding: 'utf8' },
    ).trim();
    git(repo, 'update-ref', ref, forged); // 攻击者：不使用 --force 也能强推本地 ref

    await page.reload({ waitUntil: 'load' });
    await page.waitForSelector('.alarm', { timeout: 15000 });
    const alarm = await page.evaluate(() => {
      const el = document.querySelector('.alarm');
      return {
        title: el.querySelector('h6')?.textContent ?? '',
        shape: el.querySelector('.shape')?.className ?? '',
        hasEvidence: !!document.querySelector('#alarm-log .kv'),
      };
    });
    c.ok(alarm.title.includes('改写'), `告警封条出现：「${alarm.title}」`);
    c.ok(alarm.shape.includes('burst'), '告警 → 形状 burst');
    c.ok(alarm.hasEvidence, '告警同时进入完整性页签的记录');

    await shot(page, 'e2e-alarm.png');

    // ── 10) 全程无未捕获错误 ────────────────────────────────────
    const real = consoleErrors.filter((e) => !/favicon|404/i.test(e));
    c.ok(real.length === 0, '全程无未捕获的 JS 错误', { errors: real });

    // 服务仍然活着
    const h2 = await (await fetch(base + '/api/health')).json();
    c.ok(h2.ok === true, '篡改检测之后服务仍可用（只告警，不罢工）');

    await ctx.close();
  } finally {
    proc?.kill('SIGTERM');
    await new Promise((r) => setTimeout(r, 300));
    proc?.kill('SIGKILL');
    await rm(work, { recursive: true, force: true });
  }
}
