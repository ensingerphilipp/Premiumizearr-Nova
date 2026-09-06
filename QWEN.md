# QWEN.md — Qwen operating instructions

Read `PROJECT.md` (goals, scope, change policy) and `ARCHITECTURE.md`
(architecture, approved decisions, gate details) before any work. They are
authoritative; this file is a concise operating checklist.

## Verification

- `scripts/verify` (repo root) is the mandatory local deterministic verification command. Run
  it after any change; do not land a change that makes it worse.
- Do not weaken the gate, use `-vet=off`, add suppressions/exclusions, or edit
  product source files merely to keep it green without fixing the underlying issues. 
  Fix in-scope formatting and code defects without weakening verification or changing intended behavior. Report unrelated 
  or pre-existing failures and route them according to the task and approval boundaries.
  Gate failures are reported, not silently fixed.
- Do not modify `scripts/verify`, `.github/workflows/verify.yml`, or the
  contract documents without explicit human approval (contract changes
  require HITL).

## Change policy (summary)

- Autonomous work: bug fixes (failing-first regression test where reasonably
  feasible), behavior-preserving hygiene, test/verification improvements.
- HITL required: major dependencies / the Go `go` directive, architecture
  changes, breaking API/UI/CLI changes, security model, persistence, contract
  changes, removal or breaking change of `pkg/` exports (public Go module),
  net-new product features (must originate from an approved objective/issue).
- Race fixes: minimal local synchronization only — reproduce →
  `go test -race` → narrow fix; no unrelated concurrency refactoring in the
  same change; no restructuring of config propagation or service boundaries.

## Tests

- Every bug fix must add a failing-first regression test where reasonably
  feasible. If deterministic reproduction is not reasonably feasible, document
  why and provide the strongest deterministic verification available.
- Tests must be deterministic: `httptest` fakes for premiumize.me/*arr HTTP,
  `t.TempDir` for filesystem, no network, no reliance on real config or host
  binaries.
- No coverage floor; test meaningful behavior.

## Environment expectations

- Locally provisioned Go >= 1.24.2 (verification runs with `GOTOOLCHAIN=local`;
  go.mod keeps floor `go 1.23.0` / `toolchain go1.24.2`); Node 22; npm; a C
  compiler (for `-race`).
- `scripts/verify` checks all of the above and fails with actionable errors;
  it never installs toolchains and never rewrites `go.mod`. If a prerequisite
  is missing on this machine, report it — do not silently skip.

## Repository notes

- Keep contract/verification work separate from product code work in its own branch/PR.
- Commit style: imperative, concise (see `git log`).
