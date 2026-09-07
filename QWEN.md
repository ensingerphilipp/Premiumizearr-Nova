# QWEN.md — Premiumizearr-Nova project instructions

Before work, read the repository contracts:

- `PROJECT.md` — product goals, scope, HITL boundaries, public contracts, support policy.
- `ARCHITECTURE.md` — component boundaries, technical decisions, toolchains, CI and runtime constraints.
- `.qwen/review-rules.md` — project-specific semantic review invariants.

The host-global Qwen engineering rules remain applicable. This file adds project context; it does not duplicate or weaken the global baseline.

## Verification

The repository's deterministic verification authority is:

```bash
bash scripts/verify
```

Run it after relevant changes and before handoff. Exact mechanical checks belong in `scripts/verify`, not in this file.

## Project-specific boundaries

Respect the approval boundaries and public API/security/persistence contracts in `PROJECT.md`. Treat toolchain and architecture decisions in `ARCHITECTURE.md` as project truth.

Do not invent missing project policy. Surface material ambiguity for human resolution.
