# ImmuLog

**A chat application built on Git, with verifiable history.**

[![CI](https://github.com/rech114/ImmuLog/actions/workflows/ci.yml/badge.svg)](https://github.com/rech114/ImmuLog/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24-00ADD8.svg)](go.mod)
[![Go dependencies](https://img.shields.io/badge/Go%20dependencies-0-brightgreen.svg)](go.mod)

Read this in [中文](README.zh.md).

---

ImmuLog stores each message as a Git commit. Every node keeps a local copy of the history, so a relay can still deny service or stop syncing, but a rewrite of history becomes detectable to nodes that already hold the earlier state.

The web UI presents the same history as a chat. Messages can be signed, feeds can be synchronized through ordinary Git remotes, and peers can compare their views to detect conflicting histories.

## Features

- Git-backed append-only message feeds
- Signed commits with Git SSH signing
- Local witness anchors for detecting rewritten history
- Multi-remote synchronization with quarantine and fast-forward-only updates
- Snapshot gossip: whole-view comparison with peers, which reaches a feed a relay never showed you
- Split-view detection between peers
- External anchoring through a small HTTP publisher interface
- Optional message-body encryption with per-epoch keys
- Retraction as an appended event rather than deletion
- Single binary deployment with an embedded frontend
- No third-party Go dependencies and no external frontend requests at runtime

## Quick start

Requires **Go 1.24+** and the **`git` executable** on `PATH`.

```bash
git clone https://github.com/rech114/ImmuLog.git
cd ImmuLog
go build -o immulog .
```

ImmuLog reads the local Git identity:

```bash
git config --global user.name  "Your Name"
git config --global user.email "you@example.com"
```

Start the server:

```bash
./immulog
```

The default address is http://localhost:8081 and the default repository directory is `./repoDB`.

The frontend is embedded into the binary, so Node.js is not required to run a node.

## Signing

Signing is optional, but without a signing key the Git author field is not an authenticated identity.

Configure Git to sign commits with an SSH key:

```bash
git config --global gpg.format ssh
git config --global user.signingkey ~/.ssh/id_ed25519.pub

mkdir -p ~/.config/immulog
printf '%s %s\n' "you@example.com" "$(cat ~/.ssh/id_ed25519.pub)" \
  > ~/.config/immulog/allowed_signers

git config --global gpg.ssh.allowedSignersFile ~/.config/immulog/allowed_signers
```

If a signing key is configured but Git cannot use it, ImmuLog refuses to start instead of silently creating unsigned messages.

When no signing key is configured, the startup log and the Integrity page report that the identity can be impersonated.

## Key rotation

A signing-key change must be announced by a commit signed with the old key.

```bash
ssh-keygen -t ed25519 -C you@example.com -f ~/.ssh/new_key
ssh-keygen -lf ~/.ssh/new_key.pub

curl -XPOST localhost:8081/api/rotate \
  -d '{"key":"SHA256:..."}'

git config --global user.signingkey ~/.ssh/new_key.pub
```

The rotation notice links the old and new keys. A key change without a valid rotation notice is rejected by the synchronization checks.

## Multiple nodes

Any Git repository that supports fetch and push can be used as a relay.

For example, using a bare repository:

```bash
git init --bare /srv/chat.git

IMMULOG_REPO=./a \
IMMULOG_REMOTES=hub=/srv/chat.git \
PORT=8081 \
./immulog

IMMULOG_REPO=./b \
IMMULOG_REMOTES=hub=/srv/chat.git \
PORT=8082 \
./immulog
```

Each node publishes its own feed and fetches feeds from its configured remotes.

Network data is fetched into quarantine refs first. A feed is promoted only after its history passes the local checks and the update can be applied as a fast-forward.

When two remotes provide incompatible histories for the same feed, ImmuLog reports a split view instead of choosing one silently.

That comparison is per feed, so it cannot see a feed a source simply never mentions. Gossip covers that gap: point `IMMULOG_PEERS` at a few nodes and each one's whole view is compared with theirs once per interval.

```bash
IMMULOG_PEERS="alice=http://10.0.0.7:8082,carol=http://10.0.0.9:8082" ./immulog
```

A view costs one request, so gossip can reach peers you do not fetch from. It is **read-only**: nothing a peer claims is ever promoted into `refs/feeds/*`. A feed only peers can see is listed under Peers, never raised as an alarm. And with fewer than three peers a disagreement cannot be attributed to either side, which the server says at startup.

## Message-body encryption

Encryption is disabled by default.

```bash
IMMULOG_ENCRYPT=1 ./immulog
```

When enabled, message bodies are encrypted before being stored as Git objects. Epoch keys are stored separately from the repository and wrapped for recipients using their public encryption keys.

| Data | Stored where |
|------|--------------|
| Message body | Encrypted Git object |
| Epoch key | Local key store |
| Wrapped recipient keys | Git metadata |
| Author, timestamp, sequence number, OID | Public metadata |

Each epoch uses a separate key. A new member is only given access to epochs created after they joined, and compromising a later epoch key does not reveal earlier epochs.

## Key shredding

An epoch key can be discarded:

```bash
curl -XPOST localhost:8081/api/shred \
  -d '{"epoch":1}'
```

The encrypted objects and their Git history remain. The local key is discarded, and a corresponding event is recorded in the history.

This provides crypto-shredding, not physical deletion.

## Encryption limits

The current implementation is not anonymous and does not hide Git metadata.

In addition, the ImmuLog node decrypts message bodies before sending them to its browser client. Running a node therefore does not provide the same confidentiality model as a client-to-client end-to-end encrypted chat system.

Discarding a key also cannot remove copies that another participant already decrypted or stored.

See [DESIGN.md §6.5](docs/DESIGN.md) for the complete model and limitations.

## How it works

A feed is a Git commit chain:

```
refs/feeds/<feed>

A
│
B
│
C
```

Each message is a commit object. The feed ref points to the current tip.

The ref itself can be moved, so every node also keeps a local witness of the last tip it accepted:

```
refs/witness/<feed>
```

The witness is local state. It is never pushed or fetched.

A normal update moves the feed forward:

```
A → B → C → D
```

A rewrite or rollback produces a tip that is no longer a descendant of the witness:

```
A → B → C
       \
        X
```

The local check can then report the inconsistency without trusting the remote to describe what happened.

### Write path

```
Browser
   │
   │ POST
   ▼
internal/web
   │
   ▼
core/feed
   │
   ├── build message
   ├── sign / encrypt
   ├── create Git commit
   └── update feed with CAS
   │
   ▼
core/gitx
   │
   ▼
git
```

### Sync path

```
remote
   │
   ▼
refs/quarantine/*
   │
   ├── verify against local witness
   ├── compare with other remotes
   └── verify key chain
   │
   ▼
fast-forward only
   │
   ▼
refs/feeds/*
```

The important boundaries are:

| Rule | Purpose |
|------|---------|
| `core/gitx` is the only package that uses `os/exec` | Keep Git process handling in one place |
| Network refs enter quarantine first | Remote input never writes trusted refs directly |
| Feed updates are fast-forward-only | Rewrites do not silently replace local history |
| Witness refs are local | A remote cannot rewrite the detector |
| Retractions are appended events | Retracting a message does not rewrite the past |
| Commit metadata uses Git trailers | Human-readable data and machine-readable metadata share the same object |

## Why Git

Most of the integrity model comes directly from Git primitives.

Create a commit without a worktree or index:

```bash
git commit-tree <tree> -p <parent>
```

Update a ref with compare-and-swap semantics:

```bash
git update-ref refs/feeds/alice <new> <old>
```

Check whether a history is still a descendant of a known point:

```bash
git merge-base --is-ancestor <witness> <tip>
```

Read all feed tips in one operation:

```bash
git for-each-ref \
  --format='%(refname)%1f%(objectname)' \
  refs/feeds/
```

ImmuLog keeps these operations behind `core/gitx` instead of implementing Git object storage or transport itself.

## Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `IMMULOG_REPO` | `./repoDB` | Local bare Git repository |
| `PORT` | `8081` | HTTP listen port |
| `IMMULOG_REMOTES` | unset | Comma-separated Git sync sources, using `url` or `name=url` |
| `IMMULOG_ANCHOR_URL` | unset | Optional external anchoring service |
| `IMMULOG_SYNC_INTERVAL` | `5s` | Synchronization interval |
| `IMMULOG_PEERS` | unset | Comma-separated gossip peers, `url` or `name=url`, where the URL is an HTTP base such as `http://host:8082` |
| `IMMULOG_GOSSIP_INTERVAL` | `5s` | Snapshot gossip interval |
| `IMMULOG_ANCHOR_INTERVAL` | `60s` | External anchoring interval |
| `IMMULOG_ENCRYPT` | unset | Set to `1` to enable message-body encryption |

When optional security features are not configured, the server reports that state at startup.

For example:

```
WARN no IMMULOG_REMOTES: single-node mode, no peer sync
WARN no IMMULOG_ANCHOR_URL: anchors stay local, not truly external
WARN no user.signingkey: messages are unsigned, identity can be impersonated
```

## HTTP API

| Method | Path | Purpose |
|--------|------|---------|
| `GET` | `/` | Embedded frontend |
| `GET` | `/api/stream` | SSE event stream |
| `POST` | `/api/commit` | Append a message or event |
| `POST` | `/api/epoch` | Create a new encryption epoch |
| `POST` | `/api/shred` | Discard an epoch key |
| `POST` | `/api/rotate` | Announce a signing-key rotation |
| `GET` | `/api/snapshot` | Current view of all feeds |
| `GET` | `/api/health` | Liveness and identity information |

The event stream uses commit OIDs as SSE event IDs. Browsers reconnect with `Last-Event-ID` and resume from the corresponding point in the stream.

## Project layout

```
immulog/
├── go.mod
├── main.go
├── docs/
│   └── DESIGN.md
├── core/
│   ├── gitx/
│   │   ├── exec.go
│   │   ├── object.go
│   │   ├── ref.go
│   │   └── transport.go
│   └── feed/
│       ├── feed.go
│       ├── key.go
│       ├── verify.go
│       ├── snapshot.go
│       ├── sync.go
│       ├── gossip.go
│       ├── anchor.go
│       ├── crypto.go
│       ├── epoch.go
│       └── keyring.go
├── internal/
│   └── web/
│       ├── http.go
│       ├── sse.go
│       └── guard.go
├── web/
│   ├── index.html
│   ├── style.css
│   └── app/
│       ├── main.js
│       ├── stream.js
│       ├── api.js
│       ├── store.js
│       ├── render.js
│       └── mock.js
└── tools/
```

`core/` is intentionally outside Go's `internal/` tree so it can be imported by external programs.

On the frontend, `stream.js` is the only file that touches `EventSource`, `api.js` is the only file that calls `fetch`, and `render.js` is the only file that touches the DOM.

On the backend, `gitx` is the only package allowed to call `os/exec`.

These boundaries are documented in more detail in [DESIGN.md](docs/DESIGN.md).

## Frontend

The frontend is plain HTML, CSS, and JavaScript using native ES modules. There is no runtime framework and no browser-side build step.

Third-party assets are vendored into `web/vendor/` and embedded into the binary. CI checks that files under `web/` do not reference external URLs.

The development checks use:

- `jsdom` for DOM and stylesheet integrity checks
- Playwright for browser checks
- axe-core for accessibility checks

These packages are development tools only. They are not part of the runtime.

## Testing

Go checks:

```bash
go vet ./...
go test ./...
```

Frontend and browser checks:

```bash
cd tools
npm install

npm run smoke
npm run visual
```

CI runs three jobs:

- Go build, formatting, vet, DCO, dependency, and race checks
- jsdom smoke tests
- Chromium layout, resilience, SSE, accessibility, federation, and end-to-end checks

Screenshots and measurements are uploaded as CI artifacts.

## Development-environment limitations

The project's aarch64/proot development environment has kernel and VMA limitations that can affect `go test -race` and Git's local `receive-pack`.

The CI environment uses native x86_64 GitHub Actions runners for those checks.

The detailed reproduction and scope are documented in [DESIGN.md](docs/DESIGN.md).

## Documentation

[Design document](docs/DESIGN.md) contains the full threat model, data model, synchronization protocol, cryptographic design, frontend architecture, testing strategy, known vulnerabilities, and limitations.

[Contributing guide](CONTRIBUTING.md) contains the development rules and DCO requirements.

## Contributing

Contributions use the Developer Certificate of Origin (DCO), not a CLA.

Every commit must contain a `Signed-off-by:` line:

```bash
git commit -s
```

CI checks this requirement for the complete branch history.

See [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request.

## License

Apache License 2.0.

See [LICENSE](LICENSE) and [NOTICE](NOTICE) for the complete license text and third-party component notices.