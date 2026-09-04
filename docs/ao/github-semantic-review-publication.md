# AO GitHub semantic-review publication policy

<!-- BEGIN AO_GITHUB_SEMANTIC_PUBLICATION_V1 -->

Load this policy only when dispatching an AO semantic review or handling its terminal result. It is not a general GitHub publication policy.

## Ownership and boundaries

* `ao-pr-review` remains strictly non-posting. It must never create or modify GitHub statuses, checks, reviews, comments, labels, branches, commits, or pull requests.
* The AO orchestrator is the sole owner of the GitHub status and PR summary described here.
* This is the sole exception to the orchestrator's general prohibition on posting to GitHub.
* Publication reports semantic-review state only. It does not authorize code edits, repair work, approvals, change requests, merges, issue closure, labels, branch changes, or inline review comments.
* Never infer a verdict from task state, prose, stdout, a heartbeat, or process exit alone.
* A terminal verdict may be published only after the orchestrator has read and validated the persisted `result.json`.
* GitHub publication never changes the semantic disposition in `result.json`.

## Identity and trust requirements

Before publishing a terminal result, validate all of the following:

* `contractVersion` is supported.
* `reviewKey` exactly identifies `owner/repo#PR@EXPECTED-40-CHAR-SHA`.
* `attemptId` belongs to the monitored invocation.
* The repository and PR match the dispatched review.
* The expected, pre-review, and post-review head identities satisfy the `ao-pr-review` contract.
* `invocationTransport`, `disposition`, `semanticExitCode`, and the terminal monitor event agree with the persisted result.
* The persisted result is complete, non-empty, and from the current run directory.
* The live PR head is read again immediately before terminal publication.

If validation fails, do not publish a semantic verdict. Follow the failure handling below.

## GitHub status

Use commit-status context:

`ao/semantic-review`

Use the canonical PR URL as the target URL.

Before creating a status, read the current status for this context. If its state, description, and target URL already equal the desired values, do nothing.

After dispatch has passed the normal readiness, exact-head, and required-CI checks, publish:

* State: `pending`
* Description: `AO semantic review running for <SHORT_SHA>`

After a trustworthy terminal result, map dispositions as follows:

* `pass` → state `success`; description `AO semantic review passed for <SHORT_SHA>`
* `blocked` → state `failure`; description `AO semantic review found blocking issues for <SHORT_SHA>`
* `needs_human` → state `error`; description `AO semantic review needs attention for <SHORT_SHA>`
* `review_error` → state `error`; description `AO semantic review failed for <SHORT_SHA>`
* `stale` → state `error` on the reviewed SHA only; description `AO semantic review stale for <SHORT_SHA>`

Never publish a verdict on a SHA other than the exact reviewed SHA.

If the PR head moved, do not transfer the result to the new head. Mark only the reviewed SHA as stale and require a new exact-SHA review.

## Pull-request summary

Maintain one AO-owned PR comment containing this exact hidden marker:

`<!-- ao-semantic-review-summary:v1 -->`

Find an existing marked comment only if it was authored by the currently authenticated AO GitHub identity.

* If exactly one such comment exists, update it.
* If none exists, create it.
* If multiple matching AO-authored comments exist, stop publication and report `PUBLICATION_ERROR`; do not guess.
* Never modify another author's comment.
* Never create a new comment merely because the existing marked comment is outdated.

The visible comment must begin with:

`## AO Semantic Review`

It must concisely include:

* Current publication state: `RUNNING`, `PASS`, `BLOCKED`, `NEEDS HUMAN`, `REVIEW ERROR`, or `STALE`
* PR number and canonical URL
* Reviewed full head SHA
* Current live head SHA
* Requested and selected effort
* Selection reasons when available
* Native and base events
* Required-CI result
* Semantic disposition
* Every finding, including informational and non-blocking findings
* For each finding: stable ID, severity, confidence, summary, and source location
* `reviewKey`
* `attemptId`
* Recommended next action

Do not publish raw logs, credentials, tokens, environment variables, absolute local filesystem paths, or internal transport diagnostics. State that local evidence was retained without exposing its path.

A `PASS` summary must still preserve all non-blocking findings. `PASS` means no finding blocks the reviewed head; it does not mean that no findings exist.

## Failure handling

If the review invocation crashes, times out, is cancelled, produces no trustworthy result, or produces a malformed or identity-mismatched result:

* Never publish `success` or `failure` as a semantic verdict.
* If the dispatched repository, PR, and expected SHA remain independently trustworthy, publish state `error` on that expected SHA and update the AO summary with `REVIEW ERROR`.
* If publication identity itself cannot be established safely, make no GitHub mutation.
* Report the failure locally as `PUBLICATION_ERROR` or `REVIEW ERROR`, as applicable.
* Do not fabricate findings or reconstruct a verdict from prose.
* Do not retry publication indefinitely.

A publication failure does not invalidate a trustworthy local semantic result. Report the semantic disposition and the publication failure separately.

## Lifecycle

For each qualified exact-SHA review:

1. Validate readiness, required CI, repository identity, PR identity, and expected head.
2. Dispatch exactly one `ao-pr-review` invocation.
3. Publish or retain the idempotent `pending` status and `RUNNING` summary.
4. Monitor without busy polling.
5. Receive the terminal event.
6. Read and validate the persisted `result.json`.
7. Re-read the live PR head.
8. Publish the exact-SHA terminal status and update the single summary comment.
9. Route repair or human work according to the semantic disposition and the normal AO rules.

Do not merge automatically. A human remains responsible for the merge decision.

<!-- END AO_GITHUB_SEMANTIC_PUBLICATION_V1 -->
