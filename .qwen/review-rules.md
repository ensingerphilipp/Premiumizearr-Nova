# Review rules — Premiumizearr-Nova

This file contains only repository-specific semantic invariants that are important during review and are not fully enforced by `bash scripts/verify`.

Product scope, HITL boundaries, public contracts, security/persistence policy, and toolchain decisions are defined by `PROJECT.md` and `ARCHITECTURE.md` and must not be redefined here.

## Secrets and request handling

- Never expose premiumize.me API keys, *arr API keys, or other secrets in logs, error messages, fixtures, or review output.
- Request URLs containing credentials must not be logged.

## Public Go module surface

- Exported symbols under `pkg/` are a public Go module surface. Removal or breaking changes require the public-API assessment and human approval defined in `PROJECT.md`.

## Concurrency and runtime configuration

- Concurrency fixes must remain narrowly scoped to the affected synchronization boundary; do not restructure config propagation or service boundaries without the architecture approval required by `PROJECT.md`.
- A config-field add/change/rename must account for every relevant `ConfigUpdatedCallback` and runtime consumer, preserve/update YAML and JSON tags as appropriate, update `web/src/pages/Config.svelte` when user-configurable there, and add/update tests where feasible.

## External behavior

- Changes to premiumize.me/*arr integration behavior must preserve existing error, retry, and failure-reporting contracts unless the approved task explicitly changes them.
- Net-new product behavior and breaking API/UI/CLI changes remain subject to the approval boundaries in `PROJECT.md`.
