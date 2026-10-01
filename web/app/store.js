/* SPDX-License-Identifier: Apache-2.0 */
// store.js -- the only place that holds state.
// It must not touch the DOM and must not touch the network. Its whole surface
// is upsert / list / get / subscribe.

const byOid = new Map(); // oid -> item
const items = [];        // ordered: messages and alarms interleaved = the timeline
const listeners = new Set();

let cursors = { anchor: '', snapshot: '', anchoredAt: '', identity: null, encryption: null };
let peers = [];

const emit = (kind, payload) => {
  for (const fn of listeners) fn(kind, payload);
};

const short = (oid) => (oid || '').slice(0, 6);

// ── Writes ──────────────────────────────────────────────────────

export function upsert(evt) {
  const out = apply(evt);
  // Refresh metadata on every change, or the witness anchor count would freeze
  // at the 0 it had when `hello` arrived
  if (out || evt.type === 'hello') emit('meta', stats());
  return out;
}

function apply(evt) {
  switch (evt.type) {
    case 'msg': {
      if (byOid.has(evt.oid)) return null; // optimistic delivery and the SSE broadcast dedupe by OID
      const item = {
        kind: 'msg',
        oid: evt.oid,
        seq: evt.seq,
        author: evt.author,
        feed: evt.feed || '',
        body: evt.body,
        sig: evt.sig || '',
        epoch: evt.epoch || 0,
        locked: evt.locked === true,
        state: evt.pending ? 'pending' : (evt.verified === false ? 'unverified' : 'verified'),
      };
      byOid.set(item.oid, item);
      items.push(item);
      cursors.anchor = item.oid;
      cursors.snapshot = item.oid;
      emit('add', item);
      return item;
    }

    case 'retract': {
      const target = byOid.get(evt.retracts);
      if (!target) return null;
      // A retraction only applies to messages in the **same feed**: it is the
      // author's right. Judged only when both sides carry a feed; missing
      // information never blocks (keeps older events working).
      if (evt.feed && target.feed && evt.feed !== target.feed) return null;
      target.state = 'retracted';
      target.reason = evt.reason || '';
      target.retractOid = evt.oid;
      emit('update', target);
      return target;
    }

    case 'alarm': {
      const item = { kind: 'alarm', oid: evt.oid, ...evt };
      byOid.set(item.oid, item);
      items.push(item);
      emit('add', item);
      return item;
    }

    case 'hello':
    case 'encryption': {
      if (evt.type === 'hello') {
        cursors.anchor = evt.head || cursors.anchor;
        cursors.snapshot = evt.snapshot || cursors.snapshot;
        cursors.anchoredAt = evt.anchoredAt || cursors.anchoredAt;
        cursors.identity = evt.identity || cursors.identity;
        peers = evt.peers || peers;
      }
      if (evt.encryption) cursors.encryption = evt.encryption;
      else if (evt.type === 'encryption') cursors.encryption = evt;
      emit('meta', stats());
      return null;
    }

    default:
      return null;
  }
}

export function receive(list) {
  return list.map(upsert).filter(Boolean);
}

// Optimistic delivery lands: the placeholder becomes the real item in place, so
// the bubble the user is looking at does not jump.
export function confirm(tempOid, evt) {
  const item = byOid.get(tempOid);
  if (!item) return null;
  if (byOid.has(evt.oid)) { // the broadcast beat us here; drop the placeholder
    const i = items.indexOf(item);
    if (i >= 0) items.splice(i, 1);
    byOid.delete(tempOid);
    emit('remove', tempOid);
    return null;
  }
  byOid.delete(tempOid);
  Object.assign(item, evt, { state: 'verified' });
  byOid.set(item.oid, item);
  emit('rekey', { from: tempOid, item });
  return item;
}

// ── Reads ───────────────────────────────────────────────────────

export const list = () => items;
export const get = (oid) => byOid.get(oid);

export const stats = () => ({
  anchors: items.filter((i) => i.kind === 'msg').length,
  snapshot: short(cursors.snapshot),
  anchor: short(cursors.anchor),
  anchoredAt: cursors.anchoredAt,
  identity: cursors.identity,
  encryption: cursors.encryption,
  alarms: items.filter((i) => i.kind === 'alarm'),
  peers,
});

export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
