// api.js —— 唯一碰 fetch 的地方。
// 单一写入口：kind 区分 msg / retract / receipt，不开三个端点。

const DEMO = new URLSearchParams(location.search).has('demo');

export async function commit({ kind = 'msg', body, retracts, reason } = {}) {
  // 演示模式也走一次真实延迟，否则 pending 状态一闪而过看不见
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
    // cas_failed 必须原样暴露：它是篡改检测的信号源，不能吞
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
