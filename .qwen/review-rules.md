# Review rules — Premiumizearr-Nova

Project-specific review invariants, applied to all changes (human or agent).
`scripts/verify` enforces the mechanical subset automatically; the rest are
review-time invariants. The gate must not be weakened (contract changes
require explicit human approval).

## Mechanical gate (must pass: `bash scripts/verify`)

1. All tracked Go files gofmt-clean.
2. `go vet ./...` clean.
3. Production-style Go build succeeds (`CGO_ENABLED=0`, tags `netgo
   osusergo`, static).
4. `go test -race ./...` clean.
5. `npm ci` succeeds in `web/` (lockfile in sync with `package.json`).
6. Web production build (`npm run build`) succeeds.

## Prohibitions

- No `-vet=off`, no vet suppressions, no nolint-style comments, no exclusions,
  no weakened checks — in code or in the gate.
- No product-source edits whose only purpose is to make the gate green (gate
  failures are reported, not silently "fixed").
- No API keys or other secrets in logs, error messages, or test fixtures
  (premiumize.me key, *arr keys); request URLs that embed keys must not be
  logged.
- No removal or breaking change of exported symbols under `pkg/` without a
  recorded public-API assessment (this is a public Go module — HITL).
- No net-new product behavior that does not trace to an approved objective,
  GitHub issue, or approved planning decision.

## Change-class rules

- **Bug fixes**: add a failing-first regression test where reasonably feasible
  (if deterministic reproduction is not reasonably feasible, document why and
  provide the strongest deterministic verification available); fix narrowly (no
  unrelated refactoring in the same change).
- **Behavior-preserving hygiene**: must be behavior-preserving (formatting,
  vet defect fixes per intended behavior, test infrastructure, toolchain/CI
  alignment). Vet findings are fixed individually, never blanket-mechanical.
- **Concurrency changes**: minimal local synchronization (mutex/atomic/safe
  snapshot); must include `go test -race` evidence exercising the changed
  path; no restructuring of config propagation or service boundaries (HITL).
- **Config changes**: adding/changing/renaming a config field must identify
  and update every relevant `ConfigUpdatedCallback` / runtime consumer;
  preserve/update the YAML and JSON persistence tags as appropriate; update
  the web Config UI (`web/src/pages/Config.svelte`) when the field is
  user-configurable there; add/update tests where feasible.
- **Dependency changes**: `npm ci` must pass (lockfile updated via npm, no
  hand-edits); major dependency upgrades and the Go `go` directive require
  HITL.
- **Toolchain pin consistency**: `go.mod` (`go 1.23.0` / `toolchain
  go1.24.2`), CI (`setup-go 1.24.2`, `setup-node 22`), and the
  `scripts/verify` preflight (`GOTOOLCHAIN=local`, local Go >= 1.24.2) must
  stay consistent; `go.mod` is never silently rewritten to align numbers.

## Test quality

- Deterministic: `httptest` for HTTP (premiumize.me, *arr), `t.TempDir` for
  files, no network, avoid sleep-based timing.
- Test meaningful behavior, not coverage numbers (no coverage floor).
