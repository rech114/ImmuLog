// SPDX-License-Identifier: Apache-2.0
// check/sse.mjs —— 验证「事件 id = commit OID ⇒ 断线续传白送」这条设计。
//
// 用 node:http 起一个会主动掐断的 SSE 服务端，观察浏览器原生 EventSource
// 是否真的重连、是否真的带上了 Last-Event-ID。这是整个通信设计的核心卖点，
// 之前一次都没被验证过。

import { serve, session, shot, VIEWPORTS } from '../lib/harness.mjs';

const OID = (ch) => ch.repeat(40);
const OIDS = [OID('a'), OID('b'), OID('c')];

export default async function sse(browser, _base, c) {
  const conns = [];
  let n = 0;

  const { base, close } = await serve(undefined, {
    '/api/stream': (req, res) => {
      n += 1;
      const last = req.headers['last-event-id'] ?? null;
      conns.push({ n, lastEventId: last });

      res.writeHead(200, {
        'content-type': 'text/event-stream',
        'cache-control': 'no-cache',
        'x-accel-buffering': 'no',
      });

      // 服务端下发重连间隔，测试不必等浏览器的默认退避
      res.write('retry: 400\n\n');

      const send = (i) => res.write(
        `id: ${OIDS[i]}\nevent: msg\n` +
        `data: ${JSON.stringify({ oid: OIDS[i], seq: i + 1, author: 'srv', body: `第 ${i + 1} 条` })}\n\n`,
      );

      if (last === null) {
        send(0);
        send(1);                                  // 先给两条
        setTimeout(() => res.destroy(), 800);     // 然后掐断，逼客户端重连
      } else {
        const from = OIDS.indexOf(last) + 1;
        for (let i = from; i < OIDS.length; i += 1) send(i);
      }
    },
  });

  const { ctx, page } = await session(browser, VIEWPORTS[0]);
  const errors = [];
  page.on('pageerror', (e) => errors.push(String(e).slice(0, 200)));

  await page.goto(`${base}/`, { waitUntil: 'load' });
  await page.waitForSelector('.msg', { timeout: 20000 });
  await page.waitForTimeout(6000); // 留给掐断 + retry:400 重连 + 续传

  const dom = await page.evaluate(() => ({
    msgs: [...document.querySelectorAll('.msg')].map((el) => el.dataset.oid),
    bodies: [...document.querySelectorAll('.msg .text')].map((el) => el.textContent.trim()),
  }));

  c.ok(conns.length >= 2, `连接被掐断后自动重连（共 ${conns.length} 次连接）`, { conns });
  c.ok(conns[0]?.lastEventId === null, '首次连接不带 Last-Event-ID（全量回放）', { first: conns[0] });
  c.ok(
    conns[1]?.lastEventId === OIDS[1],
    `重连时浏览器自动带上 Last-Event-ID = 最后收到的 OID（${conns[1]?.lastEventId?.slice(0, 8)}…）`,
    { expected: OIDS[1], got: conns[1]?.lastEventId },
  );
  c.ok(dom.msgs.length === 3, `续传补齐到 3 条（实际 ${dom.msgs.length}）`, dom);
  c.ok(new Set(dom.msgs).size === dom.msgs.length, '续传没有产生重复消息', dom);
  c.ok(errors.length === 0, '重连过程没有未捕获异常', { errors });

  await shot(page, 'sse-reconnect.png');
  await ctx.close();
  close();
}
