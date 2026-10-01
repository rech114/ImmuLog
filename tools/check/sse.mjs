// SPDX-License-Identifier: Apache-2.0
// check/sse.mjs -- verifies the design claim "event id = commit OID, so
// resume-after-disconnect is free".
//
// A node:http SSE server that hangs up on purpose, to observe whether the
// browser's native EventSource really reconnects and really sends
// Last-Event-ID. This is the central claim of the whole communication design.

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

      // The server sends the reconnect interval, so the test does not wait for
      // the browser's default backoff
      res.write('retry: 400\n\n');

      const send = (i) => res.write(
        `id: ${OIDS[i]}\nevent: msg\n` +
        `data: ${JSON.stringify({ oid: OIDS[i], seq: i + 1, author: 'srv', body: `message ${i + 1}` })}\n\n`,
      );

      if (last === null) {
        send(0);
        send(1);                                  // deliver two first
        setTimeout(() => res.destroy(), 800);     // then hang up, forcing a reconnect
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
  await page.waitForTimeout(6000); // room for the hang-up + retry:400 reconnect + resume

  const dom = await page.evaluate(() => ({
    msgs: [...document.querySelectorAll('.msg')].map((el) => el.dataset.oid),
    bodies: [...document.querySelectorAll('.msg .text')].map((el) => el.textContent.trim()),
  }));

  c.ok(conns.length >= 2, `reconnects automatically after a hang-up (${conns.length} connections)`, { conns });
  c.ok(conns[0]?.lastEventId === null, 'the first connection carries no Last-Event-ID (full replay)', { first: conns[0] });
  c.ok(
    conns[1]?.lastEventId === OIDS[1],
    `the browser sends Last-Event-ID on reconnect = the last OID received (${conns[1]?.lastEventId?.slice(0, 8)}...)`,
    { expected: OIDS[1], got: conns[1]?.lastEventId },
  );
  c.ok(dom.msgs.length === 3, `resume fills the gap to 3 messages (actually ${dom.msgs.length})`, dom);
  c.ok(new Set(dom.msgs).size === dom.msgs.length, 'resuming did not duplicate any message', dom);
  c.ok(errors.length === 0, 'no uncaught exception during reconnection', { errors });

  await shot(page, 'sse-reconnect.png');
  await ctx.close();
  close();
}
