/* SPDX-License-Identifier: Apache-2.0 */
// mock.js -- referenced only by stream.js (?demo=1).
// Purpose: the backend does not exist yet, but the UI has to be viewable.
// The contract is identical to the real SSE stream.

const HEX = 'abcdef0123456789';
const oid = () => Array.from({ length: 40 }, () => HEX[(Math.random() * 16) | 0]).join('');

const GENESIS = oid();
const ALICE = oid();
const BOB = oid();
const CAROL = oid();

const script = (send) => [
  [0, { type: 'hello', head: CAROL, anchoredAt: 'today 08:00',
    // Demo mode has no real signing key -- label it unsigned rather than
    // pretending otherwise
    identity: { signed: false },
    peers: [
      { name: 'origin', url: 'git@node-a', ok: true },
      { name: 'mirror', url: 'git@node-b', ok: true },
      { name: "alice's machine", url: 'git@10.0.0.7:immulog.git', ok: false, note: 'inconsistent with the local witness anchor' },
    ],
    // The view comparison, kept apart from the git-sync picture above.
    // Peer URLs are scheme-less on purpose: `web/` may not contain an external
    // URL (CI enforces it), and the server accepts `host:port` anyway.
    gossip: [
      { name: 'origin', url: 'node-a:8082', ok: true, note: 'agrees', feeds: 4 },
      { name: 'mirror', url: 'node-b:8082', ok: true, note: 'knows feeds this node does not', missingHere: [`refs/feeds/${ALICE}`] },
      { name: 'relay', url: 'node-c:8082', ok: false, note: 'disagrees about 1 feed(s)', diverged: [`refs/feeds/${BOB}`] },
    ],
    missingFeeds: [{ feed: `refs/feeds/${ALICE}`, peers: 2 }] }],

  [700, { type: 'snapshot', digest: GENESIS, feeds: 4 }],

  [300, { type: 'msg', oid: GENESIS, seq: 1, author: 'alice', body: 'Up. This one is the genesis commit.' }],

  [900, { type: 'msg', oid: ALICE, seq: 2, author: 'alice',
    body: 'I ported the GitChat idea over, but the write path is commit-tree now.', sig: 'ssh-ed25519 ...9f2a ok' }],

  [1500, { type: 'msg', oid: BOB, seq: 3, author: 'bob',
    body: 'So the server does not even need an index any more?', sig: 'ssh-ed25519 ...4c71 ok' }],

  [2200, { type: 'msg', oid: CAROL, seq: 4, author: 'carol',
    body: 'Right, and ref updates go through CAS, so losing a race fails loudly instead of dropping a message.', sig: 'ssh-ed25519 ...ab03 ok' }],

  [3200, { type: 'retract', oid: oid(), retracts: ALICE, reason: 'got a detail wrong' }],

  [4600, { type: 'alarm', oid: oid(), title: 'History rewrite detected',
    detail: 'The alice feed served by the relay is inconsistent with the local witness anchor, and there is no rewrite notice. The local copy has been kept and will not be overwritten.',
    local: '3f2a1b...', remote: '9c8b4d...' }],

  [5600, { type: 'msg', oid: oid(), seq: 5, author: 'carol',
    body: 'This is the whole point: it can change things, but it cannot change them quietly.', sig: 'ssh-ed25519 ...ab03 ok' }],
];

export function mock({ onEvent, onLink }) {
  onLink?.('up');
  const timers = script(onEvent).map(([delay, evt]) => setTimeout(() => onEvent(evt), delay));

  // A message every 9 seconds, so it is obvious the UI is live
  let seq = 6;
  const beat = setInterval(() => {
    onEvent({ type: 'msg', oid: oid(), seq: seq++, author: 'bob',
      body: `heartbeat ${seq}`, sig: 'ssh-ed25519 ...4c71 ok' });
  }, 9000);

  return () => { timers.forEach(clearTimeout); clearInterval(beat); };
}
