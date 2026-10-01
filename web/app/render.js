// render.js —— 唯一碰 document 的地方。
// 只做三件事：追加、标记撤回、插入告警。没有 diff，因为 append-only 不需要 diff。

// 形状即状态：MD3 Expressive 的形状语汇，全部由 Beer CSS 提供（mask-image 到 SVG）
const SHAPE = {
  verified: 'gem',              // 完整、确定
  pending: 'loading-indicator', // 乐观投递中（旋转由 CSS 负责）
  unverified: 'circle',         // 未知
  retracted: 'slanted',         // 被切掉
  alarm: 'burst',               // 炸开
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
};

const shapeClass = (state) =>
  ['shape', SHAPE[state] ?? 'circle', ANIM[state]].filter(Boolean).join(' ');

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
  el.dataset.state = it.state;
  const shape = el.querySelector('.shape');
  if (shape) {
    shape.className = shapeClass(it.state);
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
  el.querySelector('.text').textContent = it.body;
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
  el.querySelector('.detail').textContent =
    `对象地址  ${it.oid}\n签名      ${it.sig || '（演示数据）'}\n状态      ${it.state}`;
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
  h.textContent = it.title || '历史被改写'; // 服务端来的字符串一律 textContent

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

  refs.alarmLog.querySelector('.placeholder')?.remove(); // 清掉初始的「无」
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
  strong.textContent = it.title || '历史被改写';
  const mono = document.createElement('p');
  mono.className = 'mono muted';
  mono.textContent = `${it.local || ''} → ${it.remote || ''}`;
  box.append(strong, mono);

  row.append(icon, box);
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
      row.className = 'kv';

      const icon = document.createElement('i');
      if (!p.ok) icon.style.color = 'var(--error)';
      icon.textContent = p.ok ? 'cloud_done' : 'cloud_off';

      const box = document.createElement('div');
      box.className = 'max';
      const name = document.createElement('strong');
      name.textContent = p.name ?? '';
      const url = document.createElement('p');
      url.className = 'small-text muted';
      url.textContent = p.url ?? '';
      box.append(name, url);

      const chip = document.createElement('span');
      chip.className = 'chip';
      chip.textContent = p.ok ? '一致' : '分歧';

      row.append(icon, box, chip);
      refs.peers.appendChild(row);
    }
  }

  if (s.anchors > 0 && !refs.room.dataset.set) refs.room.dataset.set = '1';
}

export function room(id) {
  refs.room.textContent = id;
  // 主题色 = 房间的创世哈希：两个房间同色 ⇒ 历史同源
  theme.set(`#${id}`);
}

export function link(state) {
  refs.link.dataset.state = state;
  refs.link.textContent = state === 'up' ? 'cloud_done' : 'cloud_off';
  refs.link.title = state === 'up' ? '已连接' : '连接中断，正在重连';
}

// ── 视图 / 交互 ─────────────────────────────────────────────────

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

// Beer 的 JS 是异步模块，不一定在首次需要时就绪 —— 但绝不无限轮询
const theme = (() => {
  let tries = 0;
  let pending = null;
  const apply = () => {
    if (typeof ui !== 'function') {
      if (tries++ >= 15) return; // ~1.8s 后放弃，不打扰用户
      return void setTimeout(apply, 120);
    }
    try {
      if (pending?.seed) ui('theme', pending.seed);
      ui('mode', pending?.mode ?? 'dark');
    } catch { /* 种子无效就用 Beer 默认 */ }
  };
  return {
    set(seed) { pending = { ...pending, seed }; apply(); },
    mode(m) { pending = { ...pending, mode: m }; apply(); },
  };
})();

export function mode(m) {
  document.body.classList.toggle('dark', m === 'dark');
  refs.theme.firstElementChild.textContent = m === 'dark' ? 'light_mode' : 'dark_mode';
  refs.theme.setAttribute('aria-label', m === 'dark' ? '切换到浅色' : '切换到深色');
  theme.mode(m);
}

export function toast(text) {
  refs.toastText.textContent = text;
  refs.toast.classList.add('active');
  clearTimeout(toast._t);
  toast._t = setTimeout(() => refs.toast.classList.remove('active'), 3200);
}

// main 才是滚动容器（app shell），不是 window
function stick() {
  const el = refs.main;
  if (el.scrollHeight - el.scrollTop - el.clientHeight < 240) {
    requestAnimationFrame(() => { el.scrollTop = el.scrollHeight; });
  }
}
