/* SPDX-License-Identifier: Apache-2.0 */
// render.js -- the only place that touches the DOM.
// It does exactly three things: append, mark retracted, insert an alarm.
// There is no diffing, because an append-only timeline needs none.

// Shape is state: the MD3 Expressive shape vocabulary, all provided by Beer CSS
// (mask-image onto SVG).
const SHAPE = {
  verified: 'gem',              // whole, settled
  pending: 'loading-indicator', // optimistic delivery in flight (spinning is CSS)
  unverified: 'circle',         // unknown
  retracted: 'slanted',         // cut away
  alarm: 'burst',               // blown open
};

const ANIM = { retracted: 'swap', alarm: 'swap' };

const $ = (sel) => document.querySelector(sel);

const refs = {
  main: $('main'),
  stream: $('#stream'),
  views: $('#views'),
  room: $('#room-id'),
  link: $('#link'),
  theme: $('#theme'),
  toast: $('#toast'),
  toastText: $('#toast-text'),
  input: $('#input'),
  send: $('#send'),
  peers: $('#peers'),
  alarmLog: $('#alarm-log'),
  anchorCount: $('#anchor-count'),
  anchorLine: $('#anchor-line'),
  snapshot: $('#snapshot'),
  anchoredAt: $('#anchored-at'),
  identityLine: $('#identity-line'),
  identityChip: $('#identity-chip'),
  encLine: $('#enc-line'),
  encChip: $('#enc-chip'),
};

const shapeClass = (state) =>
  ['shape', SHAPE[state] ?? 'circle', ANIM[state]].filter(Boolean).join(' ');

// ── Timeline ────────────────────────────────────────────────────

export function item(it) {
  const el = it.kind === 'alarm' ? alarmEl(it) : messageEl(it);
  refs.stream.appendChild(el);
  stick();
  return el;
}

export function update(it) {
  const el = refs.stream.querySelector(`[data-oid="${it.oid}"]`);
  if (!el) return;
  el.dataset.state = it.state;
  const shape = el.querySelector('.shape');
  if (shape) {
    shape.className = shapeClass(it.state);
    shape.dataset.state = it.state;
  }
  if (it.state === 'retracted') {
    const note = el.querySelector('.strike-note');
    if (note) note.textContent = it.reason ? `retracted · ${it.reason}` : 'retracted';
  }
}

export function rekey(from, it) {
  const el = refs.stream.querySelector(`[data-oid="${from}"]`);
  if (!el) return;
  el.dataset.oid = it.oid;
  el.dataset.state = it.state;
  const shape = el.querySelector('.shape');
  shape.className = shapeClass(it.state);
  shape.dataset.state = it.state;
  paint(el, it);
}

export function remove(oid) {
  refs.stream.querySelector(`[data-oid="${oid}"]`)?.remove();
}

function messageEl(it) {
  const el = document.createElement('div');
  el.className = 'msg';
  el.dataset.oid = it.oid;
  el.dataset.state = it.state;
  if (it.locked) el.classList.add('locked');

  el.innerHTML = `
    <div class="${shapeClass(it.state)}" data-state="${it.state}"></div>
    <div class="max">
      <div class="row no-space">
        <span class="who"></span>
        <button class="meta" aria-expanded="false"></button>
      </div>
      <p class="text"></p>
      <span class="strike-note"></span>
      <div class="detail"></div>
    </div>`;

  el.querySelector('.who').textContent = it.author;
  el.querySelector('.text').textContent = it.locked
    ? '(cannot decrypt -- the key was discarded, or you are not a recipient of this epoch)'
    : it.body;
  paint(el, it);

  const meta = el.querySelector('.meta');
  meta.addEventListener('click', () => {
    const open = el.classList.toggle('open');
    meta.setAttribute('aria-expanded', String(open));
  });
  return el;
}

function paint(el, it) {
  el.querySelector('.meta').textContent = `#${it.seq} · ${it.oid.slice(0, 6)}`;
  const enc = it.epoch
    ? `\nepoch     ${it.epoch}${it.locked ? ' (unreadable here)' : ''}`
    : '';
  el.querySelector('.detail').textContent =
    `object    ${it.oid}\nsignature ${it.sig || '(unsigned)'}${enc}\nstate     ${it.state}`;
}

function alarmEl(it) {
  const el = document.createElement('article');
  el.className = 'alarm';
  el.dataset.oid = it.oid;

  const shape = document.createElement('div');
  shape.className = shapeClass('alarm');
  shape.dataset.state = 'alarm';

  const box = document.createElement('div');
  box.className = 'max';

  const h = document.createElement('h6');
  h.textContent = it.title || 'History rewritten'; // server strings always go through textContent

  const p = document.createElement('p');
  p.className = 'small-text no-margin';
  p.textContent = it.detail || '';

  const m = document.createElement('p');
  m.className = 'mono muted no-margin';
  m.textContent = `local ${it.local || '--'} · remote ${it.remote || '--'}`;

  const nav = document.createElement('nav');
  nav.className = 'group';
  for (const [label, cls] of [['View evidence', 'small border round'], ['Keep local copy', 'small round']]) {
    const b = document.createElement('button');
    b.className = cls;
    b.textContent = label;
    nav.appendChild(b);
  }

  box.append(h, p, m, nav);
  el.append(shape, box);

  refs.alarmLog.querySelector('.placeholder')?.remove(); // clear the initial "none"
  refs.alarmLog.prepend(evidenceEl(it));
  return el;
}

function evidenceEl(it) {
  const row = document.createElement('div');
  row.className = 'kv';

  const icon = document.createElement('i');
  icon.style.color = 'var(--error)';
  icon.textContent = 'report';

  const box = document.createElement('div');
  box.className = 'max';
  const strong = document.createElement('strong');
  strong.textContent = it.title || 'History rewritten';
  const mono = document.createElement('p');
  mono.className = 'mono muted no-margin';
  mono.textContent = `${it.local || ''} -> ${it.remote || ''}`;
  box.append(strong, mono);

  row.append(icon, box);
  return row;
}

// ── Metadata ────────────────────────────────────────────────────

export function meta(s) {
  refs.anchorCount.textContent = s.anchors;
  refs.snapshot.textContent = s.snapshot || '--';
  refs.anchoredAt.textContent = s.anchoredAt || 'not anchored yet';
  refs.anchorLine.textContent = s.anchor ? `latest anchor ${s.anchor}` : 'not established';

  // Signing identity: when unsigned, say plainly that impersonation is possible
  // rather than glossing over it
  const id = s.identity;
  if (id) {
    refs.identityChip.textContent = id.signed ? 'signed' : 'unsigned';
    refs.identityChip.classList.toggle('error', !id.signed);
    refs.identityLine.textContent = id.signed
      ? `signing key ${id.key || '(reading)'}`
      : 'no user.signingkey configured, identity can be impersonated';
  }

  // Encryption epoch: a lost key does not mean the message never existed, so
  // this reports readability, not existence
  const enc = s.encryption;
  if (enc) {
    if (!enc.enabled) {
      refs.encChip.textContent = 'off';
      refs.encLine.textContent = 'bodies are plaintext in the git objects; anyone with a copy can read them';
    } else {
      refs.encChip.textContent = `epoch ${enc.epoch}`;
      refs.encChip.classList.toggle('error', !enc.held);
      refs.encLine.textContent = enc.held
        ? `${enc.members} recipient(s) · ciphertext replicates with the repo, only members can open it`
        : 'the key was discarded here: the ciphertext is still on the chain, but nobody can open it';
    }
  }

  refs.peers.innerHTML = '';
  for (const p of s.peers ?? []) {
    const row = document.createElement('div');
    row.className = 'kv';

    const icon = document.createElement('i');
    if (!p.ok) icon.style.color = 'var(--error)';
    icon.textContent = p.ok ? 'cloud_done' : 'cloud_off';

    const box = document.createElement('div');
    box.className = 'max';
    const name = document.createElement('strong');
    name.textContent = p.name ?? '';
    const url = document.createElement('p');
    url.className = 'small-text muted no-margin';
    url.textContent = p.note || p.url || '';
    box.append(name, url);

    const chip = document.createElement('span');
    chip.className = 'chip';
    chip.textContent = p.ok ? 'agrees' : 'differs';

    row.append(icon, box, chip);
    refs.peers.appendChild(row);
  }

  if (s.anchors > 0 && !refs.room.dataset.set) refs.room.dataset.set = '1';
}

export function room(id) {
  refs.room.textContent = id;
  // Theme colour = the room's genesis hash: two rooms of the same colour share
  // the same origin
  theme.set(`#${id}`);
}

export function link(state) {
  refs.link.dataset.state = state;
  refs.link.textContent = state === 'up' ? 'cloud_done' : 'cloud_off';
  refs.link.title = state === 'up' ? 'connected' : 'disconnected, reconnecting';
}

// ── Views and interaction ───────────────────────────────────────

export function view(name) {
  for (const s of document.querySelectorAll('.view')) s.classList.toggle('active', s.dataset.view === name);
  for (const b of refs.views.querySelectorAll('button')) b.classList.toggle('active', b.dataset.view === name);
  refs.main.scrollTop = 0;
}

export function onView(fn) {
  refs.views.addEventListener('click', (e) => {
    const b = e.target.closest('button[data-view]');
    if (b) fn(b.dataset.view);
  });
}

export function onSubmit(fn) {
  refs.send.addEventListener('click', fn);
  refs.input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault();
      fn();
    }
  });
}

export const readInput = () => refs.input.value.trim();
export const clearInput = () => { refs.input.value = ''; };
export const focusInput = () => refs.input.focus();

export function onSubmitTheme(fn) {
  refs.theme.addEventListener('click', fn);
}

// Beer's JS is an async module and may not be ready the first time we need it --
// but never poll forever
const theme = (() => {
  let tries = 0;
  let pending = null;
  const apply = () => {
    if (typeof ui !== 'function') {
      if (tries++ >= 15) return; // give up after ~1.8s; do not pester the user
      return void setTimeout(apply, 120);
    }
    try {
      if (pending?.seed) ui('theme', pending.seed);
      ui('mode', pending?.mode ?? 'dark');
    } catch { /* invalid seed: fall back to Beer's default */ }
  };
  return {
    set(seed) { pending = { ...pending, seed }; apply(); },
    mode(m) { pending = { ...pending, mode: m }; apply(); },
  };
})();

export function mode(m) {
  document.body.classList.toggle('dark', m === 'dark');
  refs.theme.firstElementChild.textContent = m === 'dark' ? 'light_mode' : 'dark_mode';
  refs.theme.setAttribute('aria-label', m === 'dark' ? 'Switch to light' : 'Switch to dark');
  theme.mode(m);
}

export function toast(text) {
  refs.toastText.textContent = text;
  refs.toast.classList.add('active');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => refs.toast.classList.remove('active'), 3200);
}

// main is the scroll container (app shell), not the window
function stick() {
  const el = refs.main;
  if (el.scrollHeight - el.scrollTop - el.clientHeight < 240) {
    requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
  }
}
