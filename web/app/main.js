/* SPDX-License-Identifier: Apache-2.0 */
// main.js -- assembly. The only place that knows all four layers:
// store / stream / api / render.
// Data flow is strictly one-way: stream -> store -> render.

import * as store from './store.js';
import * as view from './render.js';
import { connect } from './stream.js';
import * as api from './api.js';

let theme = 'dark';
let roomSet = false;

// 1) Subscribe before connecting -- otherwise the first-screen replay is missed
store.subscribe((kind, payload) => {
  if (kind === 'add') view.item(payload);
  else if (kind === 'update') view.update(payload);
  else if (kind === 'rekey') view.rekey(payload.from, payload.item);
  else if (kind === 'remove') view.remove(payload);
  else if (kind === 'meta') view.meta(payload);
});

// 2) Connect. The event id is the commit OID, and the browser supplies `since`
//    automatically via Last-Event-ID.
connect({
  onLink: view.link,
  onEvent(evt) {
    if (evt.type === 'msg' && !roomSet) {
      roomSet = true;
      view.room(evt.oid.slice(0, 6)); // room identity = the genesis commit hash
    }
    store.upsert(evt);
  },
});

// 3) Interaction
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

  // Optimistic delivery: put it on screen first, never block on the network
  const temp = `local-${Date.now()}`;
  store.upsert({ type: 'msg', oid: temp, seq: store.stats().anchors + 1, author: 'me', body: text, pending: true });
  view.clearInput();
  view.focusInput();

  const res = await api.commit({ kind: 'msg', body: text });

  if (res.ok) {
    store.confirm(temp, {
      type: 'msg', oid: res.oid, seq: res.seq || store.stats().anchors,
      author: 'me', body: text, sig: 'ssh-ed25519 ... (local key)', verified: true,
    });
  } else {
    // cas_failed is the tamper-detection signal and must be visible to the user
    view.update(store.get(temp) ? { ...store.get(temp), state: 'unverified' } : { oid: temp, state: 'unverified' });
    view.toast(`Not confirmed: ${res.error}`);
  }
});
