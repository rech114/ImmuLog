# Contributing to ImmuLog

## Licensing and the DCO

**All code is under the Apache License 2.0** (see `LICENSE`); third-party
components are listed in `NOTICE`. The authoritative per-file declaration is the
`SPDX-License-Identifier: Apache-2.0` header at the top of every source file.

### Every commit must be signed off

Commits must carry a [Developer Certificate of Origin](DCO) (DCO 1.1) sign-off:

```bash
git commit -s            # adds the Signed-off-by line automatically
git rebase --signoff     # retrofits existing commits
```

CI checks that every commit has a `Signed-off-by:` line and rejects the branch
if any is missing.

> **Note: we use a DCO, not a CLA.**
> That means **you keep the copyright to your contribution**, and the project
> **cannot be relicensed without your consent**. This is deliberate — it matches
> the project's decentralised stance.
> If you ever want to sell commercial exceptions, you would need a CLA instead.
> Having chosen the DCO, that door is closed.

> **The DCO is not a licence, and the two are independent.** It attests where a
> contribution came from; it says nothing about which licence the project uses,
> and a project on Apache-2.0, MIT or the GPL can carry one. So this file is not
> a leftover from the layered-licensing arrangement the project once had — that
> arrangement was simplified away, and the DCO's reason (provenance attested per
> commit, and copyright never taken from a contributor) was never part of it.
> The two get conflated because they arrived in the same commit. They were not
> answering the same question then either.

---

## Development

```bash
go build -o immulog .          # zero third-party dependencies
go vet ./... && go test ./...  # the full suite
```

Frontend and end-to-end checks (needs Node):

```bash
cd tools && npm install
node smoke.mjs      # jsdom: logic chain + stylesheet integrity
node run.mjs        # Chromium: layout/shapes/resilience/SSE/a11y/e2e/federation
```

## Architectural constraints

Two rules that **mirror each other**. Keep them intact when changing things:

> **Backend: `core/gitx` is the only package in the project allowed to import
> `os/exec`.** Anywhere else, an `exec.Command` call is a design defect.

> **Frontend: `web/app/stream.js` is the only file allowed to touch
> `EventSource`.**

Four disciplines follow from those (details in `docs/DESIGN.md` §10):

1. Arguments are always `[]string` and bodies always go through stdin — never
   build a shell string
2. Every git call carries a `context` timeout
3. **Never `git commit`**, only `commit-tree` (no index, no lock)
4. **Never call git inside a loop** — one call fetches a page

## Always add `no-margin` to a new `<p>`

Beer CSS ships a global rule with specificity `(0,4,1)` that puts
`margin-block-start: 1rem` on every `<p>` following a sibling; your `(0,1,1)`
rule cannot override it. See `docs/DESIGN.md` §8.9.

## Before opening a PR

- [ ] `gofmt -l .` prints nothing
- [ ] `go vet ./...` and `go test ./...` pass
- [ ] if you touched the frontend, run `node smoke.mjs` locally
- [ ] every commit carries `Signed-off-by`
- [ ] new files carry the correct SPDX header
