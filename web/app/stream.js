/* SPDX-License-Identifier: Apache-2.0 */
// stream.js -- the only place that touches EventSource.
// Contract: connect({ since, onEvent }) drives first-screen replay and live
// pushes through the same callback.
// The event id is the commit OID, so resume-after-disconnect is free
// (Last-Event-ID).

import { mock } from './mock.js';

const TYPES = ['hello', 'msg', 'retract', 'alarm', 'encryption', 'snapshot'];

const params = new URLSearchParams(location.search);
const DEMO = params.has('demo');
// ?poll=1 asks the server for batches instead of a held stream: the fallback for
// a proxy that buffers text/event-stream (docs/DESIGN.md §7.8). A server that
// hangs up after each batch is something EventSource already knows how to resume
// from, so this is the only line of frontend code poll mode needs.
const POLL = params.has('poll');

export function connect({ since, onEvent, onLink } = {}) {
  if (DEMO) return mock({ onEvent, onLink });

  const q = new URLSearchParams();
  if (since) q.set('since', since);
  if (POLL) q.set('poll', '1');
  const qs = q.toString();
  const es = new EventSource(qs ? `/api/stream?${qs}` : '/api/stream');

  // In poll mode the connection ending is the normal end of a cycle, not a lost
  // link -- so the indicator stays on 'poll' rather than blinking down every
  // few seconds, which would be a worse lie than saying nothing.
  es.onopen = () => onLink?.(POLL ? 'poll' : 'up');
  es.onerror = () => onLink?.(POLL ? 'poll' : 'down');

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
