/* SPDX-License-Identifier: Apache-2.0 */
// stream.js -- the only place that touches EventSource.
// Contract: connect({ since, onEvent }) drives first-screen replay and live
// pushes through the same callback.
// The event id is the commit OID, so resume-after-disconnect is free
// (Last-Event-ID).

import { mock } from './mock.js';

const TYPES = ['hello', 'msg', 'retract', 'alarm', 'encryption', 'snapshot'];
const DEMO = new URLSearchParams(location.search).has('demo');

export function connect({ since, onEvent, onLink } = {}) {
  if (DEMO) return mock({ onEvent, onLink });

  const url = since ? `/api/stream?since=${encodeURIComponent(since)}` : '/api/stream';
  const es = new EventSource(url);

  es.onopen = () => onLink?.('up');
  es.onerror = () => onLink?.('down'); // the browser reconnects on its own, with Last-Event-ID

  for (const type of TYPES) {
    es.addEventListener(type, (e) => {
      try {
        onEvent({ type, oid: e.lastEventId, ...JSON.parse(e.data) });
      } catch {
        /* ignore a malformed frame; never break the stream over it */
      }
    });
  }

  return () => es.close();
}
