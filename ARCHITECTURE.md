# ARCHITECTURE.md — Premiumizearr-Nova

Current architecture, component boundaries, approved technical decisions, and
verified known risks. For goals/scope/change policy see `PROJECT.md`.

## System overview

A single Go daemon (`premiumizearrd`) serving a Svelte web UI from the same
HTTP server. Two transfer pipelines plus a configuration/status API:

```
*arr (Sonarr/Radarr/Lidarr)
  │ blackhole: .nzb/.magnet/.torrent files
  ▼
BlackholeDirectory ──(fsnotify watch, or poll every N min)──▶
  DirectoryWatcherService ──▶ stringqueue ──▶ processUploads (single goroutine)
                                                   │ premiumize.me /transfer/create (multipart)
                                                   ▼
                                              premiumize.me
  │ transfers land in the TransferDirectory folder (default "arrDownloads",
  │ auto-created on Premiumize)
  ▼
TransferManagerService (15s loop)
  ├── transfers in error status → fuzzy-matched against *arr history
  │     → history item marked failed in the *arr + transfer deleted
  └── completed folders → recursive download via `wget` subprocess
        → DownloadsDirectory/<folder>/
        → Premiumize folder deleted on success; 30-min in-memory cooldown on failure
        (TransferOnlyMode disables this side entirely)

WebServerService: gorilla/mux on bindIP:bindPort (default 0.0.0.0:8182),
serves the SPA from ./static + JSON API. No authentication by design.
```

## Component boundaries (Go)

| Component | Path | Responsibility |
|---|---|---|
| Entry / wiring | `cmd/premiumizearrd/` | Flags/env, version banner, manual wiring of config → premiumize.me client → 4 services → `ConfigUpdatedCallback` fan-out. No DI framework, no signal handling/graceful shutdown. |
| Config | `internal/config/` | YAML load/save, defaults, missing-field backfill, docker dir overrides, `UpdateConfig` (whole-struct replace + save). |
| *arr clients | `internal/arr/` | Per-arr wrappers over `golift.io/starr` (sonarr/radarr/lidarr); fuzzy filename matching; cached history; `IArr` interface. |
| Services | `internal/service/` | `TransferManagerService`, `DirectoryWatcherService`, `WebServerService`, `ArrsManagerService`. `service.go` declares a `Service` interface no service implements (dead code, see roadmap). |
| fsnotify wrapper | `internal/directory_watcher/` | Watch directory, match/callback hooks. |
| Downloader | `internal/progress_downloader/` | `wget` subprocess via `stdbuf`, regex progress parsing into `WriteCounter`. |
| Helpers | `internal/utils/` | Docker detection, writeability probe, env defaults, folder-ID resolution; (dead: `Unzip`, `StringInSlice`). |
| Premiumize.me API | `pkg/premiumizeme/` | REST client (API key in query string). Legacy zip-API methods unused. **Public Go module surface.** The only package with tests. |
| Queue | `pkg/stringqueue/` | Mutex string queue. |
| (stub) | `pkg/clouddownloader/` | Empty interface stub, entirely unused. **Public Go module surface.** |

## Pipeline behavior (current, as built)

### Blackhole → Premiumize

- fsnotify watch, or polling every `PollBlackholeIntervalMinutes` when
  `PollBlackholeDirectory` is set (poll goroutine only starts if the flag is
  already true at startup — toggling live does nothing).
- Extensions `.nzb`/`.magnet`/`.torrent` only; subdirectories are **not**
  recursed (documented limitation).
- One uploader goroutine processes the queue sequentially. After a successful
  create (or "already uploaded") the blackhole file is deleted.
- Other errors: file is popped but neither re-queued nor deleted (known bug,
  see Known risks). Error classification is exact-string matching on
  `err.Error()`.

### Premiumize → local

- 15s polling loop (hardcoded in `app.go`).
- Single-file items are wrapped in a `.folder` and moved (two-phase, completed
  on the next poll).
- Folders download recursively; each file via `wget -c` (resumable),
  `--limit-rate=<DownloadSpeedLimit>M`, `--no-check-certificate` when
  `EnableTlsCheck=false` (the default).
- Concurrency capped by `SimultaneousDownloads`: `countDownloads()` counts
  active top-level folder jobs (from admission in `HandleFinishedItem` until
  the job's deferred removal); transient child-file entries do not count.
- Failed items get a 30-minute in-memory cooldown; on success the Premiumize
  folder is deleted.
- `CleanUpDownloadDirPeriod` deletes files older than 4 days from the
  downloads directory at startup.
- `TransferOnlyMode` disables this pipeline entirely.

### *arr integration

- `golift.io/starr` client per arr; history cached per
  `ArrHistoryUpdateIntervalSeconds` (default 20s), most recent 1000 records.
- Fuzzy filename matching (strips `.nzb`/`.magnet`/`.torrent`, media
  extensions, spaces/periods/dashes/underscores; lowercased).
- Errored transfer + history match → `Fail()` on the history item in the *arr
  + `DeleteTransfer` on premiumize.me (spawned as a goroutine every 15s per
  still-errored transfer — see Known risks).

## Configuration & persistence

- YAML at `<config-dir>/config.yaml` (env `PREMIUMIZEARR_CONFIG_DIR_PATH` or
  flag `-config`); auto-created from defaults; saved on load (field backfill)
  and on every API update.
- `POST /api/config` replaces the **entire** config struct, fires each
  service's `ConfigUpdatedCallback`, then saves. No field-level validation.
- Docker (`/.dockerenv` detection): blank `BlackholeDirectory` /
  `DownloadsDirectory` are overridden to `/blackhole` / `/downloads` and
  persisted.
- **No database.** Runtime state (download list, failure cooldowns, transfer
  cache) is in-memory only and lost on restart.
- Logging: logrus → console + lumberjack-rotated files
  (`premiumizearr.{general,info,error}.log`), 100MB / 1 backup / 1-day
  retention.
- API keys (premiumize.me + *arr) stored in plaintext in `config.yaml`
  (0644) and exposed via `GET /api/config`.

## External systems & runtime dependencies

- premiumize.me REST API (API key in query strings).
- Sonarr/Radarr/Lidarr via `golift.io/starr`.
- `wget` + `stdbuf` host binaries — hard runtime dependency (installed in the
  Docker image; approved current design, native downloader is a roadmap
  candidate).
- Google Fonts CDN (referenced by `index.html`).

## Web frontend

- Svelte 3 + Carbon Components, webpack 5; two tabs (Info, Config).
- `web/public/index.html` is a Go `html/template` (`WebRoot` injected) rendered
  at runtime from `./static/index.html`.
- Client-side polling of `/api/*` every 2–5s.
- Build: `web/dist` → `build/static` → served from `./static` **relative to the
  process CWD** (systemd unit sets `WorkingDirectory`, Docker sets `WORKDIR`;
  binary installs rely on convention).

## Build, release, CI

- `Makefile`: `make web` (`npm i` + webpack), `make build` (static:
  `CGO_ENABLED=0`, tags `netgo osusergo`).
- goreleaser (`.goreleaser.yaml`, `.prerelease.goreleaser.yaml`): linux/windows
  amd64/arm64 archives, deb (nfpms), GHCR Docker images (linuxserver alpine
  base, s6), manifests.
- CI: `build.yml` (release/snapshot via goreleaser on tags/PRs) — **separate
  from the verify job**.
- `verify.yml` — PR-triggered, runs `scripts/verify` (below); the
  `verify (scripts/verify)` check is a required merge check on `main`
  (rule set `main-quality-gate`).
- Version 1.5.1 is hardcoded manually (banner in `main.go`, README, git tag).

## Approved technical decisions

1. **Node standard: 22.** Pinned explicitly wherever the toolchain is used
   (CI via `actions/setup-node`); never rely on runner preinstalled defaults.
   Node 24 later as normal maintenance.
2. **Go toolchain split (intentional).** `go.mod` declares `go 1.23.0`
   (minimum project floor) + `toolchain go1.24.2` (the project Go toolchain:
   dev on Go 1.24.2, CI pins `setup-go 1.24.2`, verification runs with
   `GOTOOLCHAIN=local` and requires local Go >= 1.24.2). Do not raise the go
   directive merely to align numbers; a floor change is an explicit future
   maintenance decision (HITL).
3. **Verification gate:** one deterministic command, `scripts/verify` — spec
   below. Never weakened.
4. **Baseline policy:** one-time behavior-preserving cleanup task (defined in
   PROJECT.md) brings the gate green; no suppressions, no `-vet=off`, no
   exclusions, no weakened checks. **Executed:** completed in PR #71
   (merged 2026-08-28); the gate has been green since.
5. **Race remediation policy:** minimal local synchronization (mutex/atomic/
   safe snapshot) preserving architecture and observable behavior; treat
   races as individual bugs (reproduce → `go test -race` test → narrow fix).
   No restructuring of config propagation or service boundaries without HITL.
6. **wget as the download mechanism:** approved current design.
7. **No built-in auth:** out of current scope (roadmap candidate requiring
   HITL); the approved current security boundary is an authenticated reverse
   proxy / trusted network.
8. **`pkg/` is the public Go module API surface:** no removal/breaking change
   of exported symbols without a public-API assessment (HITL).
9. **CI verify job:** `pull_request`-triggered; the
   `verify (scripts/verify)` check is a **required merge check** on `main`
   (rule set `main-quality-gate`, added once the baseline-cleanup PR made
   verify green). A `push: main` trigger can be added later if useful
   (not yet added). The release/Goreleaser workflow stays separate and is
   untouched by the verify work.

## scripts/verify (the gate)

Bash, run from the repo root, `set -euo pipefail`. Never skips mandatory
checks; never downloads or installs toolchains (verification exports
`GOTOOLCHAIN=local`); fails with actionable errors on the first failure.

**Preflight** (each failure is actionable):

- Go: `go` present; the script exports `GOTOOLCHAIN=local` so verification
  always uses the locally provisioned toolchain and never downloads one; the
  local Go version is reported and must be >= 1.24.2 (go.mod's `go 1.23.0` /
  `toolchain go1.24.2` is never edited to make this pass).
- Node major == 22; npm present.
- C compiler resolvable (`cc`/`gcc`/`clang`) — required for `go test -race`.

**Mandatory checks, in order (any failure fails the gate):**

1. gofmt clean over tracked Go files:
   `git ls-files -z '*.go' | xargs -0 gofmt -l` → must be empty (offenders
   listed).
2. `go vet ./...` clean.
3. Production-style build:
   `CGO_ENABLED=0 go build -tags 'netgo osusergo' -ldflags '-extldflags
   "-static"' ./cmd/premiumizearrd` → temp dir.
4. `go test -race ./...` clean.
5. `cd web && npm ci` (locked dependency graph).
6. `cd web && npm run build` (production webpack build).

**Gate state:** green (the pre-existing gofmt/vet debt that failed check 1 on
the original `planning/project-contract` branch was removed by the baseline
cleanup, PR #71); enforced as a required merge check on `main`; do not
weaken.

## Known risks & limitations (verified, not yet fixed)

**Bugs (backlog; see PROJECT.md):**

1. Stranded blackhole files + exact-string error matching (pipeline above).
2. Data races:
   - `*config.Config` mutated by `POST /api/config` (`UpdateConfig`) while the
     15s poll loops read fields (`SimultaneousDownloads`, `TransferOnlyMode`,
     …).
   - `TransferManagerService.status`/`runningTask`/`lastUpdated` written
     unlocked, read by web handlers.
   - `DirectoryWatcherService.downloadsFolderID` written unlocked in `Start`,
     read under `mu` elsewhere.
   - `WebServerService.ConfigUpdatedCallback` does `srv.Close()` + `Start()`
     from a handler goroutine and overwrites the global `indexBytes`.
3. `HandleErrorTransfer` goroutine storm: spawned every 15s per still-errored,
   history-matched transfer → concurrent/repeated `Fail()` +
   `DeleteTransfer()`.

**Other risks:**

- No graceful shutdown (no signal handling; `Run` blocks forever).
- No config validation on `POST /api/config` (whole-struct replace; empty API
  keys accepted).
- CWD-relative `./static` serving (binary-install convention).
- 1000-record history cap; full-scan fuzzy matching.
- Aggressive log retention (100MB / 1 backup / 1 day).
- `config.yaml` 0644 with plaintext keys; `GET /api/config` exposes all keys
  (no auth by design — see decision 7).
- `EnableTlsCheck=false` default (wget skips certificate verification).
- EOL CI actions (`actions/checkout@v2`, `codeql-action@v2`); deb postinstall
  uses BSD `sed -i ""` syntax (fails on Linux debs).
- Stray `web/public/814.814.js` (+ LICENSE) chunk: copied into shipped static
  but referenced by nothing.
- Dead code (verifiably no internal callers; **removal not approved** without
  public-API assessment — decision 8): `pkg/clouddownloader` (empty
  interface), `utils.Unzip`, `utils.StringInSlice`,
  `premiumizeme.GenerateZipped*` (legacy zip API), `service.Service`
  interface (unimplemented).
- `io/ioutil` usage throughout (deprecated since Go 1.16).

## Roadmap candidates (each requires HITL; none pre-approved)

- Built-in authentication.
- Native Go downloader replacing wget.
- Dead-code removal after public-API assessment.
- ldflags version injection (single source of truth for the version string).
- Node 24; ubuntu-24.x runner; Svelte 5 (open renovate branches).
- Field-level validation for `POST /api/config`.
- Secret hygiene: config file permissions (e.g. 0600), no-secrets-in-logs,
  dangerous-defaults review — each proposed separately.
- Graceful shutdown / signal handling.
