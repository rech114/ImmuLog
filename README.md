# ImmuLog

**A chat where deletion is provable and tampering is impossible to hide.**

[![CI](https://github.com/rech114/ImmuLog/actions/workflows/ci.yml/badge.svg)](https://github.com/rech114/ImmuLog/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8.svg)](go.mod)
[![Dependencies](https://img.shields.io/badge/dependencies-0-brightgreen.svg)](go.mod)

Read this in [中文](README.zh.md).

---

ImmuLog stores every message as a Git commit. The hash chain does what it always
does — change one byte and every descendant hash changes — so a server that
deletes a message, rewinds a branch, or shows different people different
histories **cannot do so quietly**. Every participant holds a copy, and every
copy is a witness.

It is not a chat app that happens to use Git. It is a tamper-evidence ledger
that happens to look like a chat app.

```
$ git log --format='%h %s' refs/feeds/<you>
a3812f5 key rotation announcement
6fd8f5f the thing I actually said
0c3b1e9 hello
```

---

## The idea in one paragraph

Most systems ask you to trust the server. ImmuLog asks you to trust **arithmetic**.
Messages are objects, not rows. Identities are signatures, not usernames. Deletion
is a key you throw away, not a `DELETE` statement. And the only thing the server
can still do — lie about history — is exactly the thing that leaves a
verifiable trace.

## Why

| Ordinary chat | ImmuLog |
|---|---|
| Admin can silently delete a message | Every holder of a copy can prove it existed |
| Admin can rewind the timeline | A local witness anchor makes rewinds visible |
| Admin can show you a different history than your friend | Cross-peer snapshot comparison catches split views |
| `author: alice` is a string anyone can type | Identity is a signed commit; author is decoration |
| "Deleted" means the bytes are gone, or maybe not | Deletion is provable: the key is gone, the ciphertext is not |
| You trust the operator | You can verify, or choose not to |

The value proposition is not "no one can delete." Nobody can promise that.
It is: **no one can delete *and hide that they did*.**

## Install

Requires **Go 1.24+** and a **`git` binary** on `PATH`. That is the entire
dependency list — the Go module has no third-party imports.

```bash
git clone https://github.com/rech114/ImmuLog.git
cd ImmuLog
go build -o immulog .
```

Set up an identity (ImmuLog reads it from Git config, never from the network):

```bash
git config --global user.name  "Your Name"
git config --global user.email "you@example.com"
```

## Usage

```bash
./immulog                       # listens on :8081, repo defaults to ./repoDB
```

Open <http://localhost:8081>. That is a working single-node chat with a
tamper-evident log.

### Signing (strongly recommended)

Without a signing key, `author` is a decoration — anyone who can push can
impersonate anyone. One-time setup:

```bash
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/id_ed25519.pub

# Lets Git decide "this signature is valid, and it belongs to who it claims"
printf '%s %s\n' "you@example.com" "$(cat ~/.ssh/id_ed25519.pub)" \
  > ~/.config/immulog/allowed_signers
git config --global gpg.ssh.allowedSignersFile ~/.config/immulog/allowed_signers
```

If a key is configured but **doesn't work**, ImmuLog refuses to start rather
than quietly send unsigned messages. Silently degrading to plaintext is the
worst outcome: you would believe you were signing when you were not.

With no key configured, both the startup log and the Integrity page say plainly
that identity can be impersonated. It does not pretend to be safe.

### Multiple nodes

Any Git host works as a relay — a bare repo on a VPS, GitHub, a USB stick.

```bash
git init --bare /srv/chat.git

IMMULOG_REPO=./a IMMULOG_REMOTES=hub=/srv/chat.git PORT=8081 ./immulog
IMMULOG_REPO=./b IMMULOG_REMOTES=hub=/srv/chat.git PORT=8082 ./immulog
```

Each node pushes its own feed and pulls **every** relay, verifying each feed
against a locally-held witness before accepting anything. Configure two or more
relays and disagreement between them is reported as a split view.

### End-to-end encryption

Off by default.

```bash
IMMULOG_ENCRYPT=1 ./immulog
```

Message bodies are AEAD-encrypted before they become Git objects. Keys are
grouped into **epochs** and wrapped separately for each recipient's public key
(`refs/keys/<n>`). The plaintext key lives **only on your machine**, never in
Git, never in a ref, never synced.

```bash
# rotate to a new epoch (recipients = public keys seen recently + you)
curl -XPOST localhost:8081/api/epoch

# crypto-shred: overwrite and delete one epoch's key, leaving an on-chain notice
curl -XPOST localhost:8081/api/shred -d '{"epoch":1}'
```

Two properties come for free:

1. **Late joiners cannot read older epochs** — they are only ever wrapped into
   epochs created after they appeared. This needs no cooperation from anyone.
2. **A key leak is contained to one epoch** — holding epoch N+1 tells you
   nothing about epoch N.

And shredding leaves an **auditable public record**: anyone can see *that* you
discarded an epoch and *when*, without seeing what it protected.

> **What encryption does not do.** OIDs, authors, timestamps and sequence numbers
> stay public — this is not anonymity. A shred only removes *your* copy; other
> holders keep theirs. It does not un-read a message someone already saw, and it
> is not a physical erase on flash storage. Your node decrypts before sending to
> the browser, so a *hosted* node can read your messages. See
> [DESIGN.md §6.5](docs/DESIGN.md).

## How it works

```
                      ┌──────────────────────────────────┐
   browser ──POST──►  │  internal/web                    │
                      │    http.go   single write entry  │
   browser ◄──SSE──   │    sse.go    event stream        │
                      │    guard.go  verify/sync/anchor  │
                      └───────────────┬──────────────────┘
                                      │
                      ┌───────────────▼──────────────────┐
                      │  core/feed                       │
                      │    feed.go     message codec     │
                      │    verify.go   witness anchors   │
                      │    sync.go     multi-source sync │
                      │    snapshot.go split-view detect │
                      │    anchor.go   anchor chain      │
                      │    key.go      signing, rotation │
                      │    epoch.go    encryption epochs │
                      └───────────────┬──────────────────┘
                                      │
                      ┌───────────────▼──────────────────┐
                      │  core/gitx      the only place   │
                      │                 with os/exec     │
                      └───────────────┬──────────────────┘
                                      │
                                   git(1)
```

Four ideas carry the whole design:

| Idea | Why it works |
|---|---|
| **Messages are objects; refs are mutable** | A ref is a pointer, and pointers can be moved. So bind messages to objects and let *others* remember where the ref used to point. |
| **A local witness anchor** | `refs/witness/<feed>` is a ref that is **never pushed and never fetched**. A rewritten history stops being its descendant. One `git merge-base --is-ancestor` call is the entire detector. |
| **Push into a quarantine, verify, then advance** | Network input never writes trusted state directly. `refs/quarantine/*` first; only fast-forwards that pass verification get promoted. |
| **Retraction is an append, never a delete** | A retraction is a new commit carrying `ImmuLog-Retracts: <oid>`. The original object stays. Deletion, when it happens, is a discarded key — not a rewritten past. |

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `IMMULOG_REPO` | `./repoDB` | Repository directory (auto `git init --bare`) |
| `PORT` | `8081` | Listen port |
| `IMMULOG_REMOTES` | — | Comma-separated sync sources: `url` or `name=url`. Any Git transport works |
| `IMMULOG_ANCHOR_URL` | — | External anchoring service. Receives `POST {snapshot, at}`; the response body is recorded as a receipt |
| `IMMULOG_SYNC_INTERVAL` | `5s` | Sync interval |
| `IMMULOG_ANCHOR_INTERVAL` | `60s` | Anchoring interval |
| `IMMULOG_ENCRYPT` | — | `1` enables encryption. **Once on, it stays on** |

Anything left unconfigured is reported at startup as a warning, with what it
costs you:

```
WARN not configured IMMULOG_REMOTES: single-node mode, no peer sync
WARN not configured IMMULOG_ANCHOR_URL: anchors stay local, not truly external
WARN no user.signingkey: messages are unsigned, identity can be impersonated
```

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/` | Frontend (embedded, same-origin — zero CORS config) |
| `GET` | `/api/stream` | SSE event stream |
| `POST` | `/api/commit` | The only write entry: `{kind, body, retracts, reason}` |
| `POST` | `/api/epoch` | Rotate to a new encryption epoch |
| `POST` | `/api/shred` | Discard one epoch's key, leaving an on-chain notice |
| `POST` | `/api/rotate` | Announce a signing-key rotation |
| `GET` | `/api/snapshot` | This node's view of every feed (the gossip unit) |
| `GET` | `/api/health` | Liveness and identity |

The stream uses **commit OIDs as event IDs**, so browsers reconnect with
`Last-Event-ID` and resume exactly. Reconnection is inherited from
`EventSource`, not implemented.

## Testing

```bash
go vet ./... && go test ./...        # ~110 cases
cd tools && npm install
node smoke.mjs                       # jsdom: logic + stylesheet integrity
node run.mjs                         # Chromium: layout, shapes, resilience, SSE,
                                     # accessibility, end-to-end, federation
```

CI runs three jobs on x86_64 runners; screenshots and measurements come back as
artifacts. See [`.github/workflows/ci.yml`](.github/workflows/ci.yml).

> **Known dev-environment limits.** On aarch64 inside a proot sandbox,
> `receive-pack` (`git push`) always fails, and `-race` cannot run due to VMA
> constraints. Both are environment issues, not code issues — the reproduction
> contains no ImmuLog code. CI covers them.

## Project layout

```
immulog/
├── core/                    ← importable library
│   ├── gitx/                the only place with os/exec
│   └── feed/                messages, witnesses, sync, crypto
├── internal/web/            HTTP + SSE + background loops
├── web/                     frontend, //go:embed
├── tools/                   CI checks (jsdom + Playwright)
└── docs/DESIGN.md           full design document
```

`core/` is deliberately **not** under `internal/` — Go forbids importing
`internal/`, and a library nobody can import is not a library.

## Design document

[docs/DESIGN.md](docs/DESIGN.md) — 15 sections covering the threat model, the
data model, retraction semantics, the wire protocol, the two Beer CSS traps that
cost us a day, and an explicit list of what this system **cannot** do.

## Contributing

Contributions are accepted under the [DCO](DCO), not a CLA — you keep your
copyright, and the project cannot be relicensed without you. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## License

[Apache-2.0](LICENSE). See [NOTICE](NOTICE) for third-party components.

> **A cost we accepted on purpose.** Apache-2.0 does not require anyone to give
> improvements back. Someone may take this code, improve it, close it, and run
> it as a service without contributing a line.
>
> For this project that is close to irrelevant: the protocol is open by design
> (Git remotes plus a handful of HTTP endpoints). Anyone can reimplement it
> without touching this code. **Open protocols can't be locked down, and
> shouldn't be.**
