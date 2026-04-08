---
name: review-gate
description: Decide whether a change is ready to merge after review. Use immediately before PR creation, merge, or completion claims that depend on review having been finished.
---

# Review Gate

This skill is a final gate, not a discovery pass. Use it after review work has already happened.

## Gate Questions

The change is only ready if all of these are true:

1. Review findings have been triaged.
2. Blocking findings are fixed.
3. Deferred findings are explicitly tracked or justified.
4. Tests or verification steps were actually run.
5. Security-sensitive changes received security review.

## Decision Process

### 1. Check unresolved findings

- Are there any still-open correctness or regression issues?
- Are there any "known broken" follow-ups being hand-waved?

### 2. Check verification evidence

- Were the relevant tests run?
- Was the actual fix path verified, not just adjacent tests?
- If a migration or ops change is involved, was that verified at the right level?

### 3. Check review completeness

- Did the review cover the risky files?
- Did someone only review the happy path?
- Was a security pass omitted even though the change needed one?

## Output Format

Return one of:

- `PASS` if the change is ready
- `BLOCKED` if merge/PR should not proceed
- `PASS WITH FOLLOW-UPS` if only explicitly tracked non-blocking items remain

Then include:

1. Short reason
2. Remaining blockers or follow-ups
3. Required next action, if any

## Rules

- Do not approve work just because implementation is complete.
- Do not treat missing verification as acceptable.
- If the review evidence is incomplete, the correct result is `BLOCKED`.
