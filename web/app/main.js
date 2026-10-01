/* SPDX-License-Identifier: Apache-2.0 */
// main.js —— 组装。唯一同时知道 store / stream / api / render 的地方。
// 数据流严格单向：stream → store → render。

import * as store from './store.js';
import * as view from './render.js';
import { connect } from './stream.js';
import * as api from './api.js';

let theme = 'dark';
let roomSet = false;

// 1) 先订阅，再接流 —— 否则首屏回放会漏掉
store.subscribe((kind, payload) => {
  if (kind === 'add') view.item(payload);
  else if (kind === 'update') view.update(payload);
  else if (kind === 'rekey') view.rekey(payload.from, payload.item);
  else if (kind === 'remove') view.remove(payload);
  else if (kind === 'meta') view.meta(payload);
});

// 2) 接流。事件 id 就是 commit OID，since 由浏览器用 Last-Event-ID 自动带
connect({
  onLink: view.link,
  onEvent(evt) {
    if (evt.type === 'msg' && !roomSet) {
      roomSet = true;
      view.room(evt.oid.slice(0, 6)); // 房间身份 = 创世 commit 的哈希
    }
    store.upsert(evt);
  },
});

// 3) 交互
view.onView(view.view);
view.mode(theme);
view.focusInput();

view.onSubmitTheme(() => {
  theme = theme === 'dark' ? 'light' : 'dark';
  view.mode(theme);
});

view.onSubmit(async () => {
  const text = view.readInput();
  if (!text) return;

  // 乐观投递：先上屏，不阻塞在网络上
  const temp = `local-${Date.now()}`;
  store.upsert({ type: 'msg', oid: temp, seq: store.stats().anchors + 1, author: '我', body: text, pending: true });
  view.clearInput();
  view.focusInput();

  const res = await api.commit({ kind: 'msg', body: text });

  if (res.ok) {
    store.confirm(temp, {
      type: 'msg', oid: res.oid, seq: res.seq || store.stats().anchors,
      author: '我', body: text, sig: 'ssh-ed25519 …（本地密钥）', verified: true,
    });
  } else {
    // cas_failed 是篡改检测的信号，必须让用户看见
    view.update(store.get(temp) ? { ...store.get(temp), state: 'unverified' } : { oid: temp, state: 'unverified' });
    view.toast(`未确认：${res.error}`);
  }
});
