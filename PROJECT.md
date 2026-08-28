# PROJECT.md — Premiumizearr-Nova Project Contract

Durable project contract: goals, scope, change policy, acceptance criteria.
For the current architecture, component boundaries, and approved technical
decisions, see `ARCHITECTURE.md`. For agent operating instructions, see
`QWEN.md`.

## What this project is

Premiumizearr-Nova is a Go daemon (`premiumizearrd`) that bridges
[premiumize.me](https://www.premiumize.me) to *arr media managers (Sonarr,
Radarr, Lidarr):

- Monitors a blackhole directory for `.nzb`, `.magnet`, and `.torrent` files
  dropped by *arr clients (blackhole download clients) and pushes them to
  premiumize.me.
- Monitors and downloads completed premiumize.me transfers to a local downloads
  directory (or runs in transfer-only mode).
- Marks failed transfers in the *arr history.
- Serves a web UI + JSON API (default port 8182) for status and configuration.

It is a continuation of Jackdallas' Premiumizearr, licensed under GNU GPL v3.

## Goals

1. Reliable transfer pipeline: no lost blackhole files, no orphaned or
   incomplete downloads, no download lockups.
2. Maintained deployments: the Docker image is the primary, supported method;
   binary/deb/Windows artifacts are built but documented as unsupported.
3. A working web UI and API for status and configuration.
4. A deterministic verification gate (`scripts/verify`) that is green and
   enforced as a required CI check.

## Out of current scope (roadmap candidates requiring HITL)

These are **not** permanent product non-goals. Each requires explicit human
planning/approval before work begins:

- **Built-in authentication.** Out of current scope; the current approved
  security model is an authenticated reverse proxy / trusted network boundary
  (the service exposes API keys and has no authentication by design). Built-in
  auth is a roadmap candidate requiring HITL.
- Net-new product features without an approved objective, GitHub issue, or
  approved planning decision. Agents must not invent product features.
- Major dependency upgrades (including changes to the Go `go` directive, Node
  24, Svelte major versions, *arr client library changes).
- Architecture changes (service boundaries, config propagation, state model).
- Security-model changes (authentication, secret storage) and persistence
  changes (today: `config.yaml` + in-memory runtime state only).
- New release targets or platforms.
- Replacing the `wget`-based downloader with a native Go implementation.

## Change policy

Autonomous (agent) work is limited to:

- **Bug fixes** — every bug fix must add a failing-first regression test where
  reasonably feasible. If deterministic reproduction is not reasonably feasible,
  document why and provide the strongest deterministic verification available.
- **Behavior-preserving maintenance/hygiene** — formatting, vet defects, test
  infrastructure, toolchain alignment, verification/CI.
- **Test and verification improvements.**

Explicit human (HITL) planning/approval is required for:

- Major dependency upgrades; changes to the Go `go` directive; architecture
  changes.
- Breaking API/UI/CLI behavior changes.
- Security-model or persistence changes.
- Changes to this contract (`PROJECT.md`, `ARCHITECTURE.md`, `QWEN.md`,
  `.qwen/review-rules.md`, `scripts/verify`, `.github/workflows/verify.yml`).
- Removal or breaking change of exported symbols under `pkg/` — this is a
  **public Go module**; external consumers may exist. Lack of internal callers
  is not sufficient justification.

Invariants:

- Do not modify product source files merely to make `scripts/verify` green.
- No suppressions, no `-vet=off`, no exclusions, no weakened checks — the gate
  must not be weakened to make anything pass.
- Net-new product behavior must originate from an approved objective, GitHub
  issue, or approved planning decision.

## Toolchain policy

- `go.mod` intentionally declares `go 1.23.0` (minimum project floor) and
  `toolchain go1.24.2` (the project Go toolchain). Development, CI
  (`setup-go 1.24.2`), and verification all run on Go 1.24.2. Do not rewrite
  `go.mod` or raise the go directive merely to align numbers; a floor change
  is an explicit future maintenance decision (HITL).
- Node standard: **22**. Pinned explicitly wherever the toolchain is used (CI
  via `actions/setup-node`); never rely on runner preinstalled defaults. A move
  to Node 24 is normal maintenance, decided later.
- `scripts/verify` exports `GOTOOLCHAIN=local` and requires the locally
  provisioned Go to be >= 1.24.2; it never downloads or installs toolchains.

## Verification

`scripts/verify` (repo root) is the single deterministic verification entry
point and is mirrored by CI (`.github/workflows/verify.yml`). Gate details:
`ARCHITECTURE.md` → "scripts/verify (the gate)".

### Current gate state

`scripts/verify` is **red by design** on the `planning/project-contract`
branch: pre-existing gofmt/vet debt fails check 1 (gofmt). This is intentional
and must not be "fixed" by weakening the gate. The red state ends with the
baseline-cleanup task below.

## Pilot acceptance criteria (this task: contract + verification setup)

1. All 6 paths in place: `PROJECT.md`, `ARCHITECTURE.md`, `QWEN.md`,
   `.qwen/review-rules.md`, `scripts/verify`, `.github/workflows/verify.yml`.
2. `scripts/verify` implemented strictly per the gate spec and failing at the
   gofmt stage on this branch (proof it catches the pre-existing debt).
3. The PR verify job in place, running the same gate, red for the same
   expected reason.
4. Zero modifications to existing product source files on this branch.

## Next explicit task: baseline cleanup

Defined here; executed later as its own task/PR after this contract setup is
reviewed. **Not part of this branch.**

- gofmt the 15 currently non-clean Go files (mechanical).
- Triage and fix each `go vet ./...` finding **individually** per its intended
  behavior — not blanket-mechanical.
- Add regression tests where a finding reveals or fixes meaningful behavior.
- Add minimal test infrastructure as needed (httptest fakes for
  premiumize.me/*arr, `t.TempDir`, no network).
- **No** suppressions, `-vet=off`, exclusions, or weakened checks.
- Small reviewable commits.
- Exit criterion: `scripts/verify` green locally and in CI. Once that PR makes
  verify green, mark the verify check a **required merge check**.

## Follow-up backlog (priority selection deferred — needs explicit approval)

**CI hygiene (existing workflows):**

- Explicit `setup-node` (Node 22) in the release `build.yml` (today it relies
  on the runner preinstalled Node).
- EOL action pins: `actions/checkout@v2`, `codeql-action@v2`,
  `github-script@v5`, `goreleaser-action@v2`.
- ubuntu-24.x runner (open renovate branch).
- Optional `push: main` trigger for the verify job (post-green).

**Verified bug inventory (details: `ARCHITECTURE.md` → Known risks):**

1. Stranded blackhole files on non-"already uploaded" transfer-create errors
   (incl. limit-reached) + exact-string `err.Error()` matching.
2. Data races: shared `*config.Config` mutated by the config API while poll
   loops read it; unlocked status-field writes; web server `Close()`+`Start()`
   from a handler goroutine (global `indexBytes`).
3. `HandleErrorTransfer` spawned as a fresh goroutine every 15s per errored,
   history-matched transfer (concurrent/repeated `Fail()` +
   `DeleteTransfer()`).

## Deployment & operations notes

- Docker is the recommended deployment (see README). The service has **no
  authentication**: keep it behind an authenticated reverse proxy / trusted
  network; bind to `127.0.0.1` where possible.
- Runtime external dependencies: `wget` + `stdbuf` host binaries (installed in
  the Docker image), premiumize.me API, *arr APIs.
- State: `config.yaml` only. All runtime state (downloads in flight, failure
  cooldowns, transfer cache) is in-memory and lost on restart.
