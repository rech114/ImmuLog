/* SPDX-License-Identifier: AGPL-3.0-or-later */
// store.js —— 唯一持有状态的地方。
// 不许碰 DOM，不许碰网络。对外只有 upsert / list / get / subscribe。

const byOid = new Map(); // oid -> item
const items = [];        // 有序：msg 与 alarm 混排，就是时间线
const listeners = new Set();

let cursors = { anchor: '', snapshot: '', anchoredAt: '', identity: null };
let peers = [];

const emit = (kind, payload) => {
  for (const fn of listeners) fn(kind, payload);
};

const short = (oid) => (oid || '').slice(0, 6);

// ── 写入 ─────────────────────────────────────────────────────────

export function upsert(evt) {
  const out = apply(evt);
  // 任何一次变更都刷新元信息，否则见证锚计数会停在 hello 那一刻的 0
  if (out || evt.type === 'hello') emit('meta', stats());
  return out;
}

function apply(evt) {
  switch (evt.type) {
    case 'msg': {
      if (byOid.has(evt.oid)) return null; // 乐观投递与 SSE 广播靠 OID 去重
      const item = {
        kind: 'msg',
        oid: evt.oid,
        seq: evt.seq,
        author: evt.author,
        feed: evt.feed || '',
        body: evt.body,
        sig: evt.sig || '',
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
      // 撤回只对**同一条 feed** 内的消息生效：撤回是作者的权利。
      // 双方都带 feed 才判定；缺信息时不拦（兼容旧事件）。
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

    case 'hello': {
      cursors.anchor = evt.head || cursors.anchor;
      cursors.snapshot = evt.snapshot || cursors.snapshot;
      cursors.anchoredAt = evt.anchoredAt || cursors.anchoredAt;
      cursors.identity = evt.identity || cursors.identity;
      peers = evt.peers || peers;
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

// 乐观投递落地：临时项原地换成正身，位置不动（用户看到的气泡不会跳）
export function confirm(tempOid, evt) {
  const item = byOid.get(tempOid);
  if (!item) return null;
  if (byOid.has(evt.oid)) { // 广播已经先到了，撤掉临时的
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

// ── 读取 ─────────────────────────────────────────────────────────

export const list = () => items;
export const get = (oid) => byOid.get(oid);

export const stats = () => ({
  anchors: items.filter((i) => i.kind === 'msg').length,
  snapshot: short(cursors.snapshot),
  anchor: short(cursors.anchor),
  anchoredAt: cursors.anchoredAt,
  identity: cursors.identity,
  alarms: items.filter((i) => i.kind === 'alarm'),
  peers,
});

export function subscribe(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}
