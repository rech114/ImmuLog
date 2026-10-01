// render.js —— 唯一碰 document 的地方。
// 只做三件事：追加、标记撤回、插入告警。没有 diff，因为 append-only 不需要 diff。

// 形状即状态：MD3 Expressive 的形状语汇，全部由 Beer CSS 提供
const SHAPE = {
  verified: 'gem',                      // 完整、确定
  pending: 'loading-indicator',         // 乐观投递中（旋转由 CSS 负责）
  unverified: 'circle',
  retracted: 'slanted',                 // 被切掉
  alarm: 'burst',                       // 炸开
};

const $ = (sel) => document.querySelector(sel);

const refs = {
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
};

// ── 时间线 ───────────────────────────────────────────────────────

export function item(it) {
  const el = it.kind === 'alarm' ? alarmEl(it) : messageEl(it);
  refs.stream.appendChild(el);
  stick();
  return el;
}

export function update(it) {
  const el = refs.stream.querySelector(`[data-oid="${it.oid}"]`);
  if (!el) return;
  const shape = el.querySelector('.shape');
  el.dataset.state = it.state;
  if (shape) {
    shape.className = `shape ${SHAPE[it.state] || 'circle'}`;
    shape.dataset.state = it.state;
  }
  if (it.state === 'retracted') {
    const note = el.querySelector('.strike-note');
    if (note) note.textContent = it.reason ? `已撤回 · ${it.reason}` : '已撤回';
  }
}

export function rekey(from, it) {
  const el = refs.stream.querySelector(`[data-oid="${from}"]`);
  if (!el) return;
  el.dataset.oid = it.oid;
  el.dataset.state = it.state;
  const shape = el.querySelector('.shape');
  shape.className = `shape ${SHAPE[it.state] || 'circle'}`;
  shape.dataset.state = it.state;
  paint(el, it);
}

export function remove(oid) {
  refs.stream.querySelector(`[data-oid="${oid}"]`)?.remove();
}

function paint(el, it) {
  el.querySelector('.meta').textContent = `#${it.seq} · ${it.oid.slice(0, 6)}`;
  el.querySelector('.detail').textContent =
    `对象地址  ${it.oid}\n签名      ${it.sig || '（演示数据）'}\n状态      ${it.state}`;
}

function messageEl(it) {
  const el = document.createElement('div');
  el.className = 'msg';
  el.dataset.oid = it.oid;
  el.dataset.state = it.state;
  el.innerHTML = `
    <div class="shape ${SHAPE[it.state] || 'circle'}" data-state="${it.state}"></div>
    <div class="max">
      <div class="row no-space">
        <span class="who"></span>
        <span class="meta" title="点开查看对象地址与签名"></span>
      </div>
      <p class="text"><span class="strike-note"></span></p>
      <div class="detail"></div>
    </div>`;

  el.querySelector('.who').textContent = it.author;
  el.querySelector('.meta').textContent = `#${it.seq} · ${it.oid.slice(0, 6)}`;
  el.querySelector('.text').prepend(document.createTextNode(it.body));
  el.querySelector('.detail').textContent =
    `对象地址  ${it.oid}\n签名      ${it.sig || '（演示数据）'}\n状态      ${it.state}`;

  el.querySelector('.meta').addEventListener('click', () => el.classList.toggle('open'));
  return el;
}

function alarmEl(it) {
  const el = document.createElement('article');
  el.className = 'alarm';
  el.dataset.oid = it.oid;

  const shape = document.createElement('div');
  shape.className = 'shape burst';
  shape.dataset.state = 'alarm';

  const box = document.createElement('div');
  box.className = 'max';

  const h = document.createElement('h6');
  h.textContent = it.title || '历史被改写'; // 服务端来的字符串一律 textContent，不走 innerHTML

  const p = document.createElement('p');
  p.className = 'small-text';
  p.textContent = it.detail || '';

  const m = document.createElement('p');
  m.className = 'mono muted';
  m.textContent = `本地 ${it.local || '——'} · 远端 ${it.remote || '——'}`;

  const nav = document.createElement('nav');
  nav.className = 'group';
  for (const [label, cls] of [['查看证据', 'small border round'], ['保留本地副本', 'small round']]) {
    const b = document.createElement('button');
    b.className = cls;
    b.textContent = label;
    nav.appendChild(b);
  }

  box.append(h, p, m, nav);
  el.append(shape, box);

  refs.alarmLog.prepend(evidenceEl(it));
  return el;
}

function evidenceEl(it) {
  const row = document.createElement('div');
  row.className = 'row';

  const shape = document.createElement('div');
  shape.className = 'shape burst';
  shape.dataset.state = 'alarm';

  const box = document.createElement('div');
  box.className = 'max';

  const strong = document.createElement('strong');
  strong.textContent = it.title || '历史被改写';

  const mono = document.createElement('p');
  mono.className = 'mono muted';
  mono.textContent = `${it.local || ''} → ${it.remote || ''}`;

  box.append(strong, mono);
  row.append(shape, box);
  return row;
}

// ── 元信息 ───────────────────────────────────────────────────────

export function meta(s) {
  refs.anchorCount.textContent = s.anchors;
  refs.snapshot.textContent = s.snapshot || '——';
  refs.anchoredAt.textContent = s.anchoredAt || '尚未锚定';
  refs.anchorLine.textContent = s.anchor ? `最新锚点 ${s.anchor}` : '尚未建立';

  if (s.peers?.length) {
    refs.peers.innerHTML = '';
    for (const p of s.peers) {
      const row = document.createElement('div');
      row.className = 'row';

      const shape = document.createElement('div');
      shape.className = `shape ${p.ok ? 'gem' : 'burst'}`;
      shape.dataset.state = p.ok ? 'verified' : 'alarm';

      const box = document.createElement('div');
      box.className = 'max';
      const name = document.createElement('strong');
      name.textContent = p.name ?? '';
      const url = document.createElement('p');
      url.className = 'small-text muted';
      url.textContent = p.url ?? '';
      box.append(name, url);

      const chip = document.createElement('span');
      chip.className = 'chip small';
      chip.textContent = p.ok ? '一致' : '分歧';

      row.append(shape, box, chip);
      refs.peers.appendChild(row);
    }
  }

  if (s.anchors > 0 && !refs.room.dataset.set) {
    refs.room.dataset.set = '1';
  }
}

export function room(id) {
  refs.room.textContent = id;
  // 主题色 = 房间的创世哈希：两个房间同色 ⇒ 历史同源
  setTheme(`#${id}`);
}

export function link(state) {
  refs.link.dataset.state = state;
  refs.link.value = state === 'up' ? 100 : 30;
}

// ── 视图 / 交互 ─────────────────────────────────────────────────

export function view(name) {
  for (const s of document.querySelectorAll('.view')) s.classList.toggle('active', s.dataset.view === name);
  for (const b of refs.views.querySelectorAll('button')) b.classList.toggle('active', b.dataset.view === name);
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

export function mode(theme) {
  document.body.classList.toggle('dark', theme === 'dark');
  refs.theme.firstElementChild.textContent = theme === 'dark' ? 'light_mode' : 'dark_mode';
  if (typeof ui === 'function') ui('mode', theme);
}

export function toast(text) {
  refs.toastText.textContent = text;
  refs.toast.classList.add('active');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => refs.toast.classList.remove('active'), 3200);
}

function setTheme(seed) {
  if (typeof ui !== 'function') return void setTimeout(() => setTheme(seed), 120);
  try { ui('theme', seed); } catch { /* 种子无效就用 Beer 默认 */ }
}

function stick() {
  const near = document.documentElement.scrollHeight - window.scrollY - window.innerHeight < 220;
  if (near) requestAnimationFrame(() => window.scrollTo({ top: document.body.scrollHeight }));
}
