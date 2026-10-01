/* SPDX-License-Identifier: AGPL-3.0-or-later */
// stream.js —— 唯一碰 EventSource 的地方。
// 契约：connect({ since, onEvent }) -> 同步回放 / 实时推送同一套回调。
// 事件 id 就是 commit 的 OID，所以断线续传是白送的（Last-Event-ID）。

import { mock } from './mock.js';

const TYPES = ['hello', 'msg', 'retract', 'alarm', 'snapshot'];
const DEMO = new URLSearchParams(location.search).has('demo');

export function connect({ since, onEvent, onLink } = {}) {
  if (DEMO) return mock({ onEvent, onLink });

  const url = since ? `/api/stream?since=${encodeURIComponent(since)}` : '/api/stream';
  const es = new EventSource(url);

  es.onopen = () => onLink?.('up');
  es.onerror = () => onLink?.('down'); // 浏览器会自动重连并带上 Last-Event-ID

  for (const type of TYPES) {
    es.addEventListener(type, (e) => {
      try {
        onEvent({ type, oid: e.lastEventId, ...JSON.parse(e.data) });
      } catch {
        /* 忽略坏帧，不打断流 */
      }
    });
  }

  return () => es.close();
}
