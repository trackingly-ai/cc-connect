---
name: code-review
description: Run a practical code review over a diff or change set. Use for pull requests, completed features, risky refactors, or before claiming work is ready to merge.
---

# Code Review

Review the requested change set with a reviewer mindset. Prioritize real defects and regressions over praise.

## Goals

1. Find correctness bugs.
2. Find behavioral regressions.
3. Find risky assumptions and edge cases.
4. Check whether tests and validation are sufficient.
5. Call out documentation gaps only when they materially affect maintainability or correctness.

## Inputs

- Target diff, PR, commit range, or changed files
- Any stated requirements, issue text, or acceptance criteria
- Optional focus areas such as performance, API stability, migration safety, or security

## Review Process

### 1. Establish scope

- Identify the files that changed.
- Identify the expected behavior.
- Identify what could regress if the change is wrong.

### 2. Inspect the code path end to end

- Trace the main execution path.
- Check error handling and fallback behavior.
- Check boundary conditions, nil/empty cases, and bad input handling.
- Check whether the change is internally consistent with neighboring code.

### 3. Inspect tests

- Verify the change is covered at the right level.
- Prefer meaningful behavioral coverage over trivial coverage.
- Call out missing negative cases, upgrade/migration cases, and interaction cases.

### 4. Inspect operational risk

- Does the change alter persistence, protocols, permissions, or external interfaces?
- Does it introduce hidden rollout, cleanup, or compatibility risks?
- Does it assume data or environments that may not hold in production?

## Output Format

If you find issues, report them in severity order:

1. `Critical` for broken behavior, unsafe data loss, security bugs, or merge blockers.
2. `High` for likely regressions or clearly unsafe assumptions.
3. `Medium` for meaningful gaps or brittle behavior.
4. `Low` for minor but real quality issues.

For each finding, include:

- File and relevant function or area
- What is wrong
- Why it matters
- What scenario demonstrates the problem

If no issues are found, say so plainly and mention any remaining confidence limits such as untested branches or missing runtime validation.

## Review Rules

- Do not spend tokens on praise sections such as "What Works Well".
- Do not invent bugs without a concrete failure mode.
- Do not focus on style unless it affects readability, safety, or maintainability.
- Prefer concise, evidence-based findings.
