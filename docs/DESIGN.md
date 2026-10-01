# ImmuLog Design Document

> A chat system that treats Git as its root of trust, not merely as a database.
> The goal is not "prevent retraction" but **provable retraction**.

---

## 0. The thesis in one sentence

**Git's hashing cannot give you "no retraction". It gives you "retraction leaves evidence".**

So the requirement is not "make it impossible for an admin to delete", but:

> **No deletion, rollback or rewrite may happen quietly.**
> An attacker can deny service and can slow you down, but **cannot leave no trace**.

There is exactly one strength formula:

```
resistance to retraction = breadth of replicas x externality of anchoring
```

Hashing itself is a freebie; what is precious is that **somebody else holds that hash too**.

---

## 1. Goals and non-goals

### Goals

| # | Goal |
|---|---|
| G1 | Messages are immutable; a retraction is an **appended event**, not a delete |
| G2 | Identity comes from a **signature**; the `author` field is only a label |
| G3 | Reference rewrites (force push / rollback / split view) are **detectable by clients, and leave evidence** |
| G4 | One binary, zero third-party dependencies, self-hosted |
| G5 | Fully compatible with the Git ecosystem (`git log` *is* the chat log) |

### Non-goals

- ❌ No real-time audio/video, no large file transfer
- ❌ Not "absolutely impossible to delete" (that is unachievable, see §6.4)
- ❌ No hand-rolled cryptography, object store or HTTP stack
- ❌ We do not hide the fact that a retraction happened — **transparency is a product requirement, not a compromise**

### Language decision

**Go + the real Git CLI. Zero third-party dependencies.**

Reason: "use the real Git" drains most of the importance out of the language
choice — what remains is routing, spawning subprocesses and streaming. Go's
standard library ships a production-grade HTTP server; Rust's does not. That is
the only gap that cannot be closed.

| | Go | Rust |
|---|---|---|
| HTTP server | `net/http` (standard library) | axum + hyper + tower + **tokio** |
| Subprocess | `os/exec` | `std::process::Command` (**a tie**) |
| Third-party deps | **0** | 5 direct / 80+ transitive |

**The tie on process model is the important line**: having chosen the real Git,
Rust's performance edge is neutralised, and what is left is 0 dependencies
against 80. For a tamper-evidence project, **zero dependencies is a security
property** — the supply-chain attack surface is nil.

---

## 2. Threat model

A signed commit fixes the attacker's **ceiling**: they cannot forge content (no
author's private key) and cannot alter a byte of an existing commit (the hash
would change).

Four moves remain:

| Attack | Method | Essence |
|---|---|---|
| **Swallow** | Drop objects / never serve a ref | Make a message vanish |
| **Rewind** | Point a ref at an older commit | Rollback |
| **Swap** | Force push to a different history | Reference rewrite |
| **Lie** | Tell different clients different stories | Split view |

"Swap" and "Rewind" are the same thing underneath: **a ref goes from A to B
where B is not a descendant of A**. Collectively: a **reference rewrite**.

> **Design principle: a reference rewrite needs no prevention, only detection.**

### What the attacker cannot do

- Forge content (no private key)
- Alter the bytes of a historical commit (the hash would change)
- Tamper **without leaving a trace** (this is the entire value of the project)

---

## 3. Data model

### 3.1 The core rule

> **A message must be bound to an object, never to a ref.**

Because: **refs are mutable, objects are immutable.** Every "retraction" in Git
is equivalent to moving or deleting a ref.

### 3.2 Concept mapping

| Chat concept | Git primitive | Note |
|---|---|---|
| Message | commit object | content addressed |
| Message body | commit message (or a blob) | see §3.4 |
| **User** | **`refs/feeds/<pubkey>`** | one append-only feed each |
| Retraction | a new commit carrying a trailer | the original object is never deleted |
| Delivery receipt | a trailer referencing the other side's OID | the source of non-repudiation |
| Read marker | a ref or tag | mutable, never evidence |
| Signature | the commit's `gpgsig` header | the `author` field is decoration |

### 3.3 Why per-user feeds rather than one shared branch

The prototype had everyone write to `refs/heads/main`. The consequence: **the
second message is necessarily a non-fast-forward and is necessarily rejected**
(measured: `! [rejected] (non-fast-forward)`).

ImmuLog gives **every user their own feed**:

```
refs/feeds/<pubkey-alice>
refs/feeds/<pubkey-bob>
refs/feeds/<pubkey-carol>
```

- Inherently conflict-free: nobody writes anyone else's ref
- Inherently append-only: only fast-forwards are allowed
- Forging alice's feed requires a force push — and others hold the old copy, so
  it **splits in the open immediately**

### 3.4 The physical shape of a message

```
commit object
├── tree:        reuses the parent's tree (a message produces no files)
├── parent:      the previous commit by the same author   <- causal chain
├── gpgsig:      the author's key signature               <- identity
└── message:
      the second message                                  <- human-readable body

      ImmuLog-Seq: 2                                      <- monotonic sequence
      ImmuLog-Retracts: <OID being retracted>             <- optional
      ImmuLog-Receipt: <OID I confirm receiving>          <- optional
```

Trailers are native Git key/value syntax, extractable directly with
`%(trailers:key=...,valueonly)`: **human-readable, machine-parseable, and
visible in plain `git log`.**

**No `--allow-empty`** — that hack exists only to work around the index (see §5.1).

---

## 4. Identity and signing

### 4.1 Identity is a key, not the author field

The prototype used `git config user.name/email` as identity, which can be
impersonated at zero cost:

```
GIT_AUTHOR_NAME='admin <img src=x onerror=alert(1)>' git commit --author='admin <admin@x>'
-> author=admin <admin@x>     <- no validation whatsoever
```

ImmuLog uses **native Git commit signing**:

```bash
git config gpg.format ssh
git config user.signingkey ~/.ssh/id_ed25519.pub
git commit-tree -S <tree> -p <parent>     # signing for free, zero code
```

The signature covers the commit contents and the tree, therefore:

- Who said it is **verifiable**
- `author: Alice` is **a label for humans only**

### 4.2 The frontend cannot choose an identity

The frontend **never sends** an author field. The author is determined by the
key configured on the server. That removes the "impersonate someone else" path
structurally.

### 4.3 Signing key rotation

Rotating a key means one **key rotation notice** commit: signed by the **old**
key, declaring the new key's fingerprint in a trailer.

```
signing key rotation

ImmuLog-Kind: rotate
ImmuLog-Seq: 7
ImmuLog-Key: SHA256:<new fingerprint>
```

Peers check the whole chain against one rule:

> **A key may only change through a rotation notice that is signed by the old
> key and plainly declares the new one.**

That separates "changed keys" from "changed people": **a legitimate rotation
always leaves a trace, and a key change with no trace is an attack**, reported as
`reason=keychain` with an alarm, and the advance is refused.

Order matters: **announce first (while the config still holds the old key), then
switch config.** In ssh mode `git commit-tree -S<keyid>` resolves a key
reference rather than a fingerprint, and there is no portable way to cross-sign —
so the ordering constraint is written explicitly in `DeclareKey`'s documentation
instead of pretending it can be automated.

**One honest boundary**: if the whole prefix of the history is unsigned, the
first signing key has nothing to vouch for it (an unsigned prefix cannot be
protected after the fact). That case is accepted, and it only means **the feed
becomes verifiable from there on**.

---

## 5. The write path

### 5.1 Only `commit-tree`, never `commit`

| | `git commit` | `git commit-tree` |
|---|---|---|
| index | required | **not required** |
| worktree | required | **not required** |
| `.lock` file | produced | **not produced** |
| usable in a bare repo | ❌ | ✅ |
| concurrent writes to different refs | conflict | **inherently safe** |
| how the message is injected | argument concatenation (**dangerous**) | **stdin** (safe) |

Measured: in a bare repository `commit-tree` creates a commit with no worktree,
no index and no lock.

```bash
echo "the second message" | git commit-tree <tree> -p <parent>
```

### 5.2 Every ref update goes through CAS

```bash
git update-ref refs/feeds/alice <new> <old>
```

When `<old>` does not match, git **refuses** (measured: `exit 128, cannot lock
ref`).

> **That one line is the "local witness anchor", and it is "refuse silent
> rewrites". Native Git, zero code, atomic.**

Multi-ref updates use `git update-ref --stdin -z` (an atomic transaction).

### 5.3 The full write sequence

```
POST /api/commit
   |
   |- 1. check that a signing key exists
   |- 2. assemble trailers (seq / retracts / receipt)
   |- 3. git commit-tree -S                  -> a new OID
   |- 4. git update-ref <ref> <new> <old>    <- CAS; abort on failure
   |- 5. broadcast the SSE event
   '- 6. return 201 { oid, seq }
```

**A failure at step 4 means someone got there first, or the local state was
tampered with.** The error must be returned verbatim and **never swallowed**.

---

## 6. Retraction and integrity

### 6.1 A retraction is an appended event

```bash
git commit-tree ... <<EOF
retraction request

ImmuLog-Retracts: <target OID>
ImmuLog-Reason: posted in the wrong channel
EOF
```

- **Default UI**: rendered as `[retracted]`
- **Expandable**: anyone holding a copy can still read the original
- **Never deleted**: the original object remains

Turning this into "after retracting, even you cannot see it" would make users
switch products — **that is product suicide**.

### 6.2 Make legitimate rewrites leave a trace too

Users themselves rebase, reset and rotate keys. Reporting every reference
rewrite as an "attack" would mean daily false alarms.

**The answer**: a legitimate rewrite is a **signed rewrite notice**, using the
old key to declare "I rewrote history after `<OID>` at `<time>`".

The rule then becomes very clean:

| Situation | Verdict |
|---|---|
| notice present + signature valid + anchor matches the local witness | legitimate rewrite, accept |
| **no notice / invalid signature / anchor mismatch** | **conclusive evidence**, no hesitation |

> **This turns an attack from "disguisable as normal operation" into "must
> present a signature that cannot be produced".**

### 6.3 Four layers of integrity

| Layer | Mechanism | What it stops |
|---|---|---|
| L1 | **local witness anchor** (`update-ref` CAS + a local record) | rollback, reference rewrite |
| L2 | **monotonic sequence** (a signed `ImmuLog-Seq`) | rewinding to a legitimate older commit |
| L3 | **receipt chain** (my commit references your OID) | unilateral deletion (it leaves a dangling reference) |
| L4 | **snapshot gossip + external anchoring** | split view, global rewrite |

**L3 deserves its own paragraph**: alice's 42nd message is referenced by bob's
receipt, and bob's feed is signed by bob and copied onto your machine. To delete
it, an admin must **rewrite both alice's and bob's feeds at once** — **two
private keys they do not have**. The only thing they can do is "not serve it",
and then you immediately see a hole nobody can patch.

**L4 reuses existing wheels**: the snapshot is a Merkle root, the consistency
proof follows Certificate Transparency's STH / consistency-proof design, and
clients exchange snapshot hashes to detect a split view. Periodically hand the
snapshot hash to something like OpenTimestamps for external anchoring.

**L4's two halves are not the same mechanism, and only one of them ever leaves
the machine.** External anchoring hands a digest to somebody else's turf.
Gossip compares digests with peers this node may not even *fetch* from: a view
costs one request, so the set of parties who can contradict you stops being
bounded by the git remotes in the configuration. Breadth is the multiplier in
§0's formula, and gossip is what makes breadth cheap.

That is the whole reason it is not a second copy of the sync check. Per-feed
comparison is blind **by construction** to a feed that only one side serves:
`Sync` groups claims by ref, so a ref nobody else declares never enters its loop
and a source can omit a feed while staying invisible. Gossip compares the two
ref **sets**, which is the only way to see it.

**Two peers are not enough to act on a disagreement.** Two views disagreeing say
*that* something is wrong, never *who* is wrong; the third view is what
localises it. A gossip set of one is a node talking to itself. The mechanism is
therefore worth having exactly where several people run nodes independently --
which is also why the server says so out loud when fewer than three peers are
configured.

Discovery is not an alarm. A feed only peers can see is reported, never raised
as a tampering alarm: one peer knowing about it is a member joining, and crying
wolf is how a real warning gets ignored.

### 6.4 Boundaries that must be stated honestly

| Limit | Explanation |
|---|---|
| Private key leak | The owner can rewrite their own history forever; signatures cannot save you. Only key rotation plus a revocation notice can |
| Timestamps are untrustworthy | commit date is the local clock. Only external anchoring means anything |
| Full rewrite + every replica offline | Unsolvable. The ceiling is the breadth of replica distribution |
| New user bootstrap | **Cannot verify the whole history.** They must trust a checkpoint signed by a set of independent witnesses |
| SHA-1 | Practical collisions exist. Git ships a hardened implementation; the SHA-256 repository format has not completed interoperable migration, so `object-format` must stay configurable |

**New-user bootstrap is a trust assumption that has to be written down, not a
vulnerability.** Tell the user in the UI: "the integrity of your history is
vouched for by these N witnesses, and you can become one to reduce that
dependency".

### 6.5 Encryption epochs and "forgettability"

Up to here the design contains a **hard conflict**:

> We promise "never delete" — that is integrity. But users will always want to
> take something back — that is privacy.

In plaintext that conflict is **unsolvable**: you must pick one. The answer is
not to pick one, but to **make deletion unnecessary**.

#### The mechanism

```
body    -> AEAD encryption (key from the epoch) -> ciphertext into a git object (freely copyable)
key     -> grouped into epochs, wrapped once per recipient's public key -> refs/keys/<n>
plaintext key -> stored only on this machine; never in git, never synced
```

"Forgetting" means **discarding that epoch's plaintext key**. The ciphertext
remains, the hash chain stays intact, the replicas remain — but nobody can open
it. That is crypto-shredding.

#### Per-recipient wrapping satisfies the two opposite demands

| | What it wants |
|---|---|
| Integrity | replicas **as wide as possible** |
| Privacy | key distribution **as narrow as possible** |

Wrapping the epoch key once per member's public key (X25519 + HKDF-SHA256 +
AES-GCM, all standard library) satisfies both: **the ciphertext replicates freely
with the repository, and only members can open it.** Every wrap uses a fresh
ephemeral key, so two wraps of the same key are not comparable — an outsider
cannot tell which members are in the same room.

#### Two properties that come for free

1. **A late joiner cannot read epochs created before they joined.** They are only
   ever wrapped into later epochs — and this **needs nobody's cooperation**; it is
   guaranteed unilaterally.
2. **A key leak is contained to one epoch.** Holding epoch N+1's key tells you
   nothing about epoch N.

#### "Discard" must be a decision, not a file deletion

The first implementation missed this: after deleting the local key file,
`openEpochKey` **unwrapped it again from the copy on the chain** (you are a
recipient, after all), so the deletion was a no-op. Discarding must therefore
leave a **persistent marker**, and `SaveEpochKey` must refuse to write back an
epoch that was discarded.

By the same logic, the `IsShredded` check must come **before** the "not found
locally, so look on the chain" fallback.

#### Discarding is auditable

`ShredEpoch` appends an `ImmuLog-Kind: shred` notice to the chain. Anyone can
see **who discarded which generation, and when** — while seeing nothing of what
it protected.

That is "provable forgetting": deletion *and* verifiability. Neither is
achievable alone.

#### What it cannot do (stated plainly)

| | |
|---|---|
| **Does not hide that a message existed** | OID, author, timestamp and sequence number all remain. **This is not anonymity** |
| **Discards only this machine's copy** | Copies elsewhere do not vanish — unless **every holder does the same** |
| **Does nothing about someone who already read it** | It protects against future readers (later clones, restored backups) |
| **Is not a physical erase** | Overwrite-plus-delete is no guarantee on journaling filesystems or flash wear levelling. The ceiling is custody discipline |
| **The node can read plaintext** | Your node holds your private key, so it decrypts before sending over SSE. Using someone else's hosted node means they can read it |

#### What encryption costs

The server loses all content capability: no indexing, no search, no moderation.
All of that has to move to the client. That is the price of privacy, and there
is no discount.

---

## 7. How the frontend talks to the backend

> This section answers: **how does a browser connect to the backend?**

### 7.1 The answer: two asymmetric channels

```
                  +--------------------------------+
  upstream (rare) |  plain HTTP POST               |
  browser ------->|  POST /api/commit              |
                  |  <- returns { oid, seq }       |
                  +--------------------------------+

                  +--------------------------------+
  downstream (hot)|  SSE  (text/event-stream)      |
  browser <-------|  GET /api/stream               |
                  |  <- long-lived, server push    |
                  +--------------------------------+
```

**Why asymmetric?** Because the two directions are fundamentally different:

| | Upstream | Downstream |
|---|---|---|
| Frequency | low (a human typing) | high (everyone talking) |
| Needs | **acknowledgement, idempotence, retries, error codes** | low latency, broadcast, resume |
| Right wheel | **HTTP** | **SSE** |

Using WebSocket for the upstream means **reinventing HTTP's status codes,
idempotence and retries yourself**. Reusing HTTP here is "reuse the wheel" in
its most literal form.

### 7.2 Upstream: two endpoints

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | `index.html` (embedded; **same-origin, so zero CORS config**) |
| `GET` | `/api/stream` | SSE event stream (downstream plus first screen) |
| `POST` | `/api/commit` | **the only write entry** |
| `GET` | `/api/snapshot` | the current snapshot digest (for peer gossip) |

`POST /api/commit` body:

```json
{ "kind": "msg", "body": "hello" }
{ "kind": "retract", "retracts": "<oid>", "reason": "posted in the wrong channel" }
{ "kind": "receipt", "refs": ["<oid>", "<oid>"] }
```

Responses:

```json
201 { "oid": "...", "seq": 42, "at": "..." }
4xx { "error": "cas_failed", "expected": "<old>", "actual": "<new>" }
```

**`cas_failed` must reach the frontend intact** — it is the tamper-detection
signal and must not be swallowed.

**Events are told apart by `kind`, not by three endpoints.** One write entry
means the frontend needs one fetch helper, and adding a semantic never touches
routing — which satisfies "minimal" and "extensible" at the same time.

### 7.3 Downstream: SSE, zero dependencies

`text/event-stream` is a standard protocol you can implement with `net/http` and
`http.Flusher` — **no third-party library required**.

Frame format:

```
retry: 3000

id: 3f2a1b9c...
event: msg
data: {"oid":"3f2a...","author":"...","body":"hello","seq":42}

id: 7d1e0f4a...
event: retract
data: {"oid":"7d1e...","retracts":"3f2a...","reason":"posted in the wrong channel"}

event: alarm
data: {"kind":"rewrite","feed":"<pubkey>","local":"3f2a...","remote":"9c8b..."}

: ping
```

Event types:

| `event` | Meaning |
|---|---|
| `hello` | connection established, carrying head / cursor / server time |
| `msg` | a new message |
| `retract` | a retraction |
| `alarm` | **a tampering alarm** (§6.3's verdict, rendered as a system message the attacker cannot delete) |
| `snapshot` | a snapshot digest; fires only when the digest changes |
| `encryption` | an encryption epoch change (created or discarded) |

### 7.4 Resume-after-disconnect comes free

**This is the core payoff of choosing SSE over WebSocket.**

SSE's `id:` field plus the browser's `EventSource` gives you, for nothing:

1. **Automatic reconnection** (not one line of reconnect code)
2. **Cursor-based resume**: on reconnect the browser sends
   `Last-Event-ID: <last event id received>`

And our event ids **are commit OIDs**, so:

```
Last-Event-ID: 3f2a1b9c...
        |
server: git log --format=... 3f2a1b9c..<tip>
        |
exactly the missing messages, no more and no fewer
```

> **The event id is a content hash — inherently globally unique, inherently
> verifiable, inherently a resume cursor.** That is a third use for hashing,
> and it is free.

**Two cursors must not be confused** (this is a key architectural discipline):

| Cursor | Lives in | Purpose | Trust |
|---|---|---|---|
| `Last-Event-ID` | the browser | **a performance mechanism**: resume | indirectly server-influenced, **untrusted** |
| local witness anchor | the server | **a security mechanism**: rewrite detection | held locally, **trusted** |

> **An SSE cursor must never stand in for a witness anchor.** A server can reset
> the cursor in your browser; it cannot forge your local witness record.

### 7.5 The first screen reuses the same connection

There is no separate "load history" endpoint:

```
GET /api/stream                      <- the only thing the frontend does at startup
  event: msg     x N                 <- the server replays the last N (git log -n)
  event: hello                       <- replay done, switching to live mode
  event: msg     ...                 <- deltas from here on
```

One connection, moving seamlessly from history into live, and **the frontend
never maintains a "how far have I loaded" state machine**.

Paging is the server's business: the last 50 first, and older history on scroll
via `GET /api/stream?since=<oid>&backward=1` — the same code path.

### 7.6 Optimistic delivery: keep verification off the critical path

```
user presses Enter
   |
   |- render immediately (grey, marked "unconfirmed")   <- zero latency
   |
   '- POST /api/commit
         |- 201 -> replace the placeholder with the real OID
         |- 4xx cas_failed -> mark red, "lost the race / conflicting history"
         '- network failure -> retry with exponential backoff

meanwhile the SSE stream may deliver the same message
   -> dedupe by OID (it was already rendered optimistically)
```

**The upstream acknowledgement and the downstream broadcast are independent
paths that converge by OID.** That is "verifiable, not verified" landing at the
protocol layer.

### 7.7 Heartbeat and backpressure

| Problem | Approach |
|---|---|
| An intermediary cuts an idle connection | a comment line `: ping` every 15–30s |
| A client is too slow | per-client bounded buffer; when full, drop it and let the browser reconnect and resume via `Last-Event-ID` |
| A slow client drags down the server | broadcast with non-blocking sends; **never block the broadcast** |

### 7.8 Degradation path

If some enterprise proxy buffers `text/event-stream`:

- first try `X-Accel-Buffering: no` to switch proxy buffering off
- if that fails, degrade to `GET /api/stream?poll=1`: the server returns a batch
  immediately and closes, and the browser retries at the interval set by `retry:`

**The same server code path, only the connection is not held.** The frontend's
`EventSource` code does not change at all.

### 7.9 Authentication: two kinds of trust

This is the easiest thing to conflate, and it must be kept apart:

| Layer | Mechanism | What it protects |
|---|---|---|
| **Transport** | a token generated at startup, stored in a cookie | "who may connect to my local node" |
| **Content** | **signed commits** | "alice really said this" |

> **HTTP auth protects the local service, not the repository.**
> The authenticity of a message **never depends** on HTTP authentication — even
> someone who steals your token can only send messages **they signed**. They
> cannot impersonate anyone.

The distinction matters because it demotes authentication to "anti-harassment"
rather than "security", which means the simplest implementation (a token in a
cookie) is sufficient — no OAuth, no JWT.

### 7.10 Why not WebSocket

| | SSE | WebSocket |
|---|---|---|
| Dependency | **`net/http` + `Flusher`** | `gorilla/websocket` + `x/net` |
| Auto-reconnect | **built into the browser** | write it yourself |
| Resume | **`Last-Event-ID`, built in** | design an application protocol |
| Direction | one-way | full duplex |
| Do we need full duplex | **no** (the upstream is POST) | — |

That last row is the point: **once the upstream is HTTP, full duplex is pure
waste.**

Incidentally: the prototype's WebSocket `readPump` broadcast browser messages to
everyone **without persisting them** — it needed one-way push all along. Turning
dead code into a design decision removes the whole `Hub`/`Client`/channel state
machine.

---

## 8. Interface (MD3 Expressive)

### 8.1 Visual language: three pillars, not materials

MD3 Expressive uses **shape, motion and colour** to express emotion and
hierarchy — **not texture and material**.

So the first rule of the interface is:

> **Encode state as shape, not as text.**

An immersive "audit log" skeuomorphism is an anti-pattern: it mistakes how much
information the product carries for a visual style. The right move is to make
information **visible when needed**, not permanently covering the screen.

### 8.2 Stack: Beer CSS over CDN, native ESM, no build step

| Decision | Value |
|---|---|
| CSS framework | **Beer CSS 5.0.3** (`cdn.jsdelivr.net/npm/beercss@5.0.3`) |
| Dynamic colour | `material-dynamic-colors@1.1.4` |
| JS | **native ES modules**, no bundler |
| Frontend third-party JS deps | **0** |

**Why vanilla JS is enough:**

> Frontend state is **single, one-way and append-only**. Frameworks solve
> "scattered state plus two-way binding plus diffing" — and we have no diffing
> requirement. The DOM is already an O(1) structure for appending at the end.
>
> Rendering an append-only log with a framework is using the heaviest wheel for
> the lightest job.

**Why a build chain is not allowed:** `//go:embed` is the pillar of "one binary,
zero dependencies". The moment there is an `npm run build`, you acquire a Node
runtime, a `dist/` directory to keep in sync, a dev server, and the
unanswerable question "why does a tamper-evidence product need 800 npm
packages?"

### 8.3 Shape is state

Beer CSS ships 35 M3 shapes; they become our state vocabulary:

| State | Shape | Colour | Semantics |
|---|---|---|---|
| Verified | `gem` | `--primary` | faceted, whole, settled |
| Pending | `loading-indicator` | `--tertiary` | optimistic delivery (spinning) |
| Unverified | `circle` | `--secondary` | unknown |
| Retracted | `slanted` | `--error` | cut away |
| **Tampering alarm** | `burst` | `--error` | blown open |

On retraction the shape **switches from `gem` to `slanted`** — which is exactly
what M3 shape morphing is for, and it is not decoration: **it is the product's
core narrative.**

### 8.4 The theme colour is the room's genesis hash

The dynamic colour (Material You) seed comes from **the first 6 hex digits of the
room's first commit OID**:

```js
ui('theme', `#${genesisOid.slice(0, 6)}`);
```

Therefore:

> **Two rooms with the same colour have the same history.**

This is not decorative re-skinning — **the palette is a function of the identity
hash**, carrying content addressing through to the visual layer at zero cost.

### 8.5 Three non-negotiable interaction rules

| Rule | How | The opposite |
|---|---|---|
| **Retraction leaves a trace** | strike-through in place, shape switched to `slanted`, original still readable on tap | retract and even you cannot see it -> users switch products |
| **An alarm is not a toast** | an alarm is **a notice sealed into the timeline** that never auto-dismisses | a flash that disappears = deniable |
| **Optimistic delivery** | the message appears at once (a `pending` shape) and is replaced in place when the POST lands | waiting for full verification before rendering is the one place where "optimising for security kills the product" |

**A placeholder is replaced in place** (the OID is rewritten, the DOM does not
move), so the bubble the user is looking at never jumps.

### 8.6 Frontend structure (four "only" rules)

```
web/
├── index.html              MD3 skeleton (Beer CSS from CDN)
├── style.css               only what Beer lacks: layout, shape semantics, motion, fonts
└── app/
    ├── main.js             assembly and entry (the only one that knows all four layers)
    ├── stream.js           <- the only place that touches EventSource
    ├── api.js              <- the only place that touches fetch
    ├── store.js            <- the only place that holds state
    ├── render.js           <- the only place that touches document
    └── mock.js             demo data source (?demo=1), same contract as the real SSE
```

> **No file may call across layers.**
> This is the **mirror image** of the backend rule "`gitx` is the only package
> that may import `os/exec`".

The property to verify (and it should always hold):

```
EventSource appears only in stream.js
fetch       appears only in api.js
document.   appears only in render.js
```

**Data flow is strictly one-way:** `stream -> store -> render`, and on the write
side `api -> store -> render`.

**`mock.js` exists on purpose**: it lets the interface run before the backend is
finished, and because its contract matches the real SSE exactly, connecting is
just deleting `?demo=1` — **that is the dividend of the "four only" discipline.**

### 8.7 Typography

| Use | Font | Why |
|---|---|---|
| Display | **Bricolage Grotesque** | variable (opsz/wdth/wght), has character, not overused |
| UI / body | **Roboto Flex** | MD3 Expressive's own typeface; Expressive's typographic emphasis rides its variable axes |
| Hashes (metadata only) | **Martian Mono** | semi-condensed technical mono, appearing only in `.meta` / `.detail` |

**Hash information is collapsed by default**: tapping `.meta` expands `.detail`
to reveal the full OID and signature — §8.1's principle, applied.

### 8.8 The CDN supply-chain problem — solved by vendoring

**A tamper-evidence product pulling CSS from a third-party CDN at runtime is a
supply-chain hole.**

This is not pedantry: the whole argument of the project is "do not trust third
parties", and `cdn.jsdelivr.net` is a third party that can change your interface
at any moment.

**This is now closed.** `tools/vendor.sh` pulls every asset into `web/vendor/`,
rewrites the absolute URLs to local siblings, and the whole directory goes into
the binary through `//go:embed`. CI fails if any file under `web/` references an
external URL again.

| Stage | Approach |
|---|---|
| Fetching | `tools/vendor.sh` (pinned versions, re-runnable) |
| Runtime | **local only** — no request leaves the machine |
| Guard | a CI step greps `web/` for `http(s)://` and fails on a hit |

What is vendored, and what it costs:

| Item | Size |
|---|---|
| `beer.min.css` | 88 KB |
| `beer.min.js` + `material-dynamic-colors.min.js` | 71 KB |
| 38 shape SVGs (`gem.svg`, `burst.svg`, ...) | ~40 KB |
| 4 Material Symbols icon fonts (woff2) | ~1.6 MB |
| 26 web font files (woff2) | ~2.1 MB |
| **Total** | **~3.8 MB, 72 files** |

The binary went from 9.7 MB to 13.6 MB. That is the price of not trusting anyone
at runtime, and it is worth paying.

Two caveats that survive vendoring:

1. **The shape SVGs are external files** (`mask-image: url(gem.svg)`), so
   vendoring the CSS alone would break every shape. The script fetches them all,
   and `check/shapes.mjs` verifies each one resolves.
2. Beer CSS does not emit `-webkit-mask` prefixes, so **shapes may not render on
   old Safari** (modern versions support the unprefixed property).

Licence texts travel with the assets: `web/vendor/beer/LICENSE` (MIT) and
`web/vendor/fonts/LICENSE` plus `NOTICE` (SIL OFL 1.1, with per-family copyright
notices).

### 8.9 Two Beer CSS traps worth remembering

| # | Symptom | Real cause | Fix |
|---|---|---|---|
| 1 | a subtitle sits a character too far below its heading | Beer ships a global rule with specificity `(0,4,1)` putting `margin-block-start: 1rem` on **every `<p>` that follows a sibling**; `.kv p` at `(0,1,1)` cannot beat it | use Beer's own escape hatch `:not([class*=margin])` — add `no-margin` to every `<p>`, then set spacing explicitly |
| 2 | the whole key/value panel stacks vertically | a line of **`// comment`** in `style.css` (not valid CSS) was parsed as part of a selector, so the `.kv { display:flex }` rule **never took effect** | only `/* */` is legal. `smoke.mjs` now guards this: brace balance, no `//` in selectors, critical rules must be present in the parsed stylesheet |

The second is especially vicious: **a syntax error reports nothing, it just
silently deletes a rule.** Combined with trap 1 it looks like "you overcorrected"
— **so the diagnosis has to come from parsed output, not from squinting at a
screenshot.**

> **Always add `no-margin` when introducing a new `<p>`.**

### 8.10 How this is verified: no browser locally, CI does it all

The development machine is aarch64, where installing Chromium risks instruction
set problems. So **every browser check runs on GitHub Actions' x86_64 runners**,
with screenshots and measurements returned as artifacts. CI usually finishes in
under 2:30.

The checks under `tools/`, filtered by "is this actually necessary here":

| Check | Responsibility | Dependency |
|---|---|---|
| `smoke.mjs` | jsdom, black-box driving the DOM; logic chain plus **stylesheet integrity** | jsdom |
| `check/layout.mjs` | geometry at 3 viewports x 3 tabs: horizontal overflow / clipping / content flush to edges / touch targets / safe margins / key-value rows on one line | playwright |
| `check/shapes.mjs` | do shape `mask-image` URLs really resolve to reachable SVGs; do icons render as glyphs rather than degrading into words | playwright |
| `check/resilience.mjs` | degradation when Beer's JS/CSS fails or the write endpoint returns 500 | playwright |
| `check/sse.mjs` | a self-hosted SSE server that hangs up, to verify automatic reconnection and `Last-Event-ID` resume | playwright + node:http |
| `check/a11y.mjs` | axe-core WCAG A/AA, keyboard operability, accessible button names | axe-core |
| `check/e2e.mjs` | the page against the real Go binary: send, render, restart behaviour | playwright + go |
| `check/federation.mjs` | two real nodes on one relay: foreign feeds sync and become trusted state; a rewritten relay is refused and alarmed on | playwright + go |
| `check/gossip.mjs` | three real nodes on two disjoint relays: the view comparison sees a feed the local relay never showed | playwright + go |

**Deliberately not adopted** (with reasons recorded):

| Candidate | Why not |
|---|---|
| `@playwright/test` | what is needed is "return the measurements", not a test runner's green/red; bare playwright suffices |
| `pixelmatch` / `BackstopJS` / Percy / Chromatic | visual regression needs a stable baseline, and the design is still moving — **no baseline means no diffing**. Screenshots sent back for a human to look at are enough |
| `@lhci/cli` | performance is not this project's bottleneck (one binary; 100k messages is about 20 MB) |
| MSW | `page.route()` already intercepts the network |

---

## 9. Project layout (one file, one responsibility)

```
immulog/
├── go.mod                   module immulog (zero third-party dependencies)
├── main.go                  assembly and lifecycle
├── docs/DESIGN.md           this document
├── core/                    <- an importable library
│   ├── gitx/                <- the only package in the project that may use os/exec
│   │   ├── exec.go          subprocess boundary: stdin injection, timeouts, error normalisation, Init
│   │   ├── object.go        commit-tree / hash-object / log / trailer reads
│   │   ├── ref.go           CAS reads and writes / for-each-ref / is-ancestor / count
│   │   └── transport.go     fetch / push (reusing git's own transports)
│   └── feed/                <- domain semantics, no I/O details
│       ├── feed.go          message codec plus send / retract / history
│       ├── key.go           signing self-test, key chain, rotation notices
│       ├── verify.go        witness anchors and reference-rewrite detection
│       ├── snapshot.go      snapshot digests and split-view verdicts
│       ├── sync.go          multi-source sync: quarantine -> verify -> fast-forward
│       ├── anchor.go        the anchor chain and the external anchoring interface
│       ├── crypto.go        epoch keys and per-recipient wrapping
│       ├── epoch.go         on-chain epoch records and lifecycle
│       └── keyring.go       this machine's key custody
├── internal/web/            <- transport, no domain logic
│   ├── http.go              routes and handlers (net/http)
│   ├── sse.go               event stream (text/event-stream)
│   └── guard.go             background loops: inspection / sync / anchoring
└── web/                     <- frontend, the whole directory is //go:embed-ed
    ├── index.html           MD3 skeleton
    ├── style.css            layout / shape semantics / motion / fonts
    └── app/
        ├── main.js          assembly (the only file that knows all four layers)
        ├── stream.js        <- the only place that touches EventSource
        ├── api.js           <- the only place that touches fetch
        ├── store.js         <- the only place that holds state
        ├── render.js        <- the only place that touches document
        └── mock.js          demo data source (?demo=1)
```

**Why `core/` is not under `internal/`**: Go forbids importing packages under
`internal/`. If `core/` is to carry an Apache-2.0 grant, it must genuinely be
`go get`-able from outside — otherwise the grant is decoration. See `LICENSE`.

**The layout is the architectural constraint** — one rule, mirrored on both
sides:

| Side | The single contact point |
|---|---|
| Backend | **`gitx` is the only package that may import `os/exec`** |
| Frontend | **`stream` is the only file that may touch `EventSource`** |

That constraint buys two things: swapping languages, swapping in libgit2 or
adding a cache layer **touches one package**; swapping the transport (SSE to
WebSocket) or the rendering approach (vanilla to a framework) **touches one
file**.

---

## 10. Engineering discipline

| # | Rule | Why |
|---|---|---|
| 1 | **arguments are always `[]string`, bodies always go through stdin** | never build a shell string, so there is no injection |
| 2 | every git call carries a `context` timeout | a stuck subprocess must not take down a handler |
| 3 | **never `git commit`**, only `commit-tree` | no index, no lock, concurrency-safe |
| 4 | **never call git inside a loop** | one `git log --format` fetches a page |
| 5 | every ref update carries `<old>` for CAS | that is the witness anchor itself |
| 6 | no `--allow-empty` | `commit-tree` does not need it |
| 7 | the UI thread touches neither Git nor cryptography | doing either synchronously drops frames |

**The only performance cost** is about 5 ms per subprocess call. The write path
is `commit-tree` + `update-ref`, roughly 10 ms — entirely adequate.

**The escape hatch**: if process overhead ever becomes a real bottleneck, swap in
libgit2 or gix — **but do not pay for a bottleneck that does not exist.** At that
point `gitx` is the only package needing a rewrite, which is exactly why it
exists.

### Reconciling performance and security

| Principle | Approach |
|---|---|
| only O(1) on the critical path | verify the signature and the tip linkage; everything else is asynchronous |
| keep the commit DAG | use `--filter=blob:none`, never `--depth` (a shallow clone destroys verification) |
| verify incrementally | verify forward from a checkpoint, so cost scales with new data |
| ref explosion | protocol v2's `ls-refs` with ref-prefix filtering |
| network | coalesce pushes and pulls — **one round trip costs as much as ten thousand signature checks** |

### Order-of-magnitude estimate

One message is about `commit(250B) + tree(60B)` (reusing the parent tree), so
roughly **300 B uncompressed**.

- 100,000 messages is about **20–30 MB**
- A year of heavy chatting fits on a phone with room to spare

**Conclusion: storage cost is negligible. The only real enemies are latency and
round trips.**

---

## 11. Roadmap

| Phase | Content | Status |
|---|---|---|
| **0** | `os/exec` + `commit-tree` + `update-ref` CAS write path | done |
| **1** | per-user feed model; trailer metadata | done |
| **2** | SSE (zero dependencies); `net/http`; **dependencies down to 0** | done |
| **3** | local witness anchor + reference-rewrite detection + `alarm` events | done |
| **4** | commit signing + key rotation notices | done |
| **5** | multi-source sync + quarantine verification + snapshots + anchor chain | done |
| **6** | epoch key encryption (crypto-shredding, i.e. forgettability) | done |
| **7** | snapshot gossip: comparing whole views, not feed by feed | done |

**Every phase runs standalone and can be rolled back.**

### Notes on multi-source sync (`feed/sync.go`)

```
for each remote:  git fetch <url> '+refs/feeds/*:refs/quarantine/<slot>/*'
                                    ^ network input never writes trusted state
for each feed:    compare with the witness -> compare with other remotes -> CAS fast-forward only
```

Three rules:

| Layer | Approach | What it stops |
|---|---|---|
| quarantine | network input lands in `refs/quarantine/*` first | a remote cannot touch trusted state directly |
| witness anchor | **foreign feeds get a witness anchor too**, and are advanced only after comparison | someone else rewriting their history is just as visible |
| fast-forward only | anything else leaves local state untouched, with an alarm | overwriting tampering |

Remotes that contradict each other (giving tips for one feed with no common
descendant) are reported as a **split view**.

A node is a git **client**: ssh / https / file / git all come free with
`git fetch` / `push`, and there is no git protocol server to implement.

### Notes on snapshots (`feed/snapshot.go`)

```go
digest = git hash-object(the sorted text of "refs/feeds/x <oid>" lines)
```

**Content addressing is already a Merkle leaf**, so there is no Merkle tree to
build.

**Why not RFC 6962 inclusion / consistency proofs**: that machinery exists for
**light clients** — they do not hold the full ref list and need O(log n) proofs.
Our clients hold every ref, so comparing them one by one is both simpler and
**stronger**. Add proofs the day light clients appear.

The bar for divergence is deliberately strict: only two tips that are **not
ancestors of one another** count as a contradiction. One side being behind is
just "not synced yet".

`Diverged` answers a narrower question than `Compare` does. `Compare` returns
four buckets: contradictions, feeds only one side serves (in either direction),
and feeds it refuses to judge because the peer's object is absent locally. Only
the first is an alarm. The other three exist because a contradicted *tip* and a
missing *feed* are different events, and the second is the one a per-feed
comparison can never reach.

### Notes on the anchor chain (`feed/anchor.go`)

Each anchor commit's parent is the previous anchor — **the chain is guarded by
git's own hash chain**: rewriting an anchor in the middle requires rewriting
every anchor after it, and clients may still hold the old ones.

```go
type Publisher interface {
    Publish(ctx, digest string) (receipt string, err error)
}
```

That is the **entire** external-anchoring interface. The real **externality**
comes from that one hop: once a receipt lives on someone else's turf, nobody can
harmonise the story afterwards. An OpenTimestamps gateway, an RFC3161 gateway or
your own notary all work — swapping services means swapping a URL.

When unconfigured, anchoring stays local and **both the service and the
interface say "not externally confirmed"**. It does not pretend to be safe.

### Authorisation of retraction (a rule added with sync)

A retraction target comes from a request body and must (1) be a valid object name
and (2) belong to the **local feed**. On the wire, retraction events also carry
`feed`, and clients apply them only **within the same feed**.

> **Retraction is the author's right, not something anyone can do to anyone.**

### Two real vulnerabilities found while building sync

1. **Trailer injection**: a single newline inside `reason` or an external receipt
   fabricates a line like `ImmuLog-Seq: 999`; and git's trailer parser takes the
   **last** occurrence — **so the forgery wins**. Fixed by `sanitizeValue`:
   trailer values are always collapsed to one line (bodies keep their newlines).
2. **Unauthorised retraction**: originally any OID could be a retraction target.
   Now the target must be an ancestor of the local tip, and the missing-object
   path in `IsAncestor` must **fail closed** (otherwise it leaked as a 500).

---

## 12. Anti-pattern list

| ❌ | Why |
|---|---|
| shallow-cloning with `--depth` for speed | no history means no ability to verify history. Use `blob:none` |
| one fetch/push per message | spending the most expensive operation (a round trip) to save the cheapest thing (bytes) |
| waiting for full verification before rendering | the one place where optimising for security kills the product |
| `git commit` / `Worktree.Pull()` | it introduces a lock, and `Pull` **merges, i.e. accepts divergence** |
| treating the `author` field as identity | plain text, impersonation at zero cost |
| trusting filesystem events for correctness | after `pack-refs` the watched directory goes **silently empty** |
| a `+` refspec | explicitly authorising a forced overwrite of local refs |
| allowing `git replace` / grafts | a legitimate backdoor for rewriting history locally, and it poisons the verification chain |
| trusting a server's reflog or a server-signed snapshot | meaningless under a split view: it can sign two of them |
| retracting so hard that even you cannot see it | users switch products |

---

## 13. Glossary

| Term | Meaning |
|---|---|
| **feed** | one append-only commit chain per user, `refs/feeds/<pubkey>` |
| **witness anchor** | the client's local record of "the last ref value I saw"; a server cannot overwrite it |
| **reference rewrite** | a ref moves from A to B where B is not a descendant of A (force push / rollback) |
| **split view** | a server shows different clients different histories |
| **rewrite notice** | the signed appended event a user must leave when legitimately rewriting history |
| **receipt chain** | my commit referencing your OID, forming a DAG that interlocks |
| **tombstone** | a retraction event: a new commit pointing at the retracted OID |
| **verifiable** | *can be* verified at any time — the goal of this project |
| **verified** | *has been* verified at all times — expensive, and unnecessary |

---

## 14. Closing

> **Zero trust at the content layer** (hashes prove themselves)
> **Multi-source consensus at the discovery layer** (local witness + gossip + external anchoring)
> **Rewrites must leave a trace** (signed notices)
> **Receipts interlock** (the DAG references itself)
> **Retraction is an append** (a tombstone; never a delete)

With those five in place, an administrator degrades into **an interchangeable
courier**: they can deny service and slow you down, but they **cannot be quiet**.

Every time they act, a system message **you generated and they cannot delete**
grows in the chat stream.

> That is why going invisible is impossible — not because they cannot change
> things, but because **changing them necessarily leaves a trace that no
> signature can cover up.**
