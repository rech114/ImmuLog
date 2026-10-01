/* SPDX-License-Identifier: Apache-2.0 */
// api.js -- the only place that touches fetch.
// A single write entry: `kind` distinguishes msg / retract / receipt instead of
// three endpoints.

const DEMO = new URLSearchParams(location.search).has('demo');

export async function commit({ kind = 'msg', body, retracts, reason } = {}) {
  // Demo mode still takes a realistic delay, or the pending state flashes by
  // unseen
  if (DEMO) {
    await new Promise((r) => setTimeout(r, 420));
    return { ok: true, oid: fakeOid(), seq: 0 };
  }

  const res = await fetch('/api/commit', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ kind, body, retracts, reason }),
  });

  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    // cas_failed must reach the surface intact: it is the tamper-detection
    // signal and must not be swallowed
    return { ok: false, error: data.error || `http_${res.status}`, ...data };
  }
  return { ok: true, ...data };
}

function fakeOid() {
  const hex = 'abcdef0123456789';
  let s = '';
  for (let i = 0; i < 40; i++) s += hex[(Math.random() * 16) | 0];
  return s;
}
