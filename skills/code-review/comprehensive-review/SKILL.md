---
name: comprehensive-review
description: Run a deeper, structured code review after implementation. Use for major features, risky refactors, release candidates, or before PR creation when a lightweight review is not enough.
---

# Comprehensive Review

Use this skill for a deliberate, multi-angle review rather than a quick pass.

## Review Axes

Review the change against all of these:

1. Correctness
2. Edge cases and failure handling
3. Maintainability
4. Security and abuse surfaces
5. Performance and scale risk
6. Compatibility and migration risk
7. Testing and verification quality

## Process

### 1. Gather review context

- Read the issue, task description, or acceptance criteria.
- Identify the changed files and their surrounding code.
- Identify any data model, API, or workflow assumptions.

### 2. Review by axis

For each axis, ask:

- What could break?
- What assumptions are hidden?
- What is not validated?
- What is missing from the tests?

### 3. Escalate specialist concerns

If the change touches authentication, authorization, secrets, database writes, migrations, or externally exposed APIs, also invoke a dedicated security-focused pass.

### 4. Summarize findings

- Group by severity, not by file order.
- Distinguish between must-fix issues and follow-up items.
- If no blocking findings exist, say whether confidence is high or limited.

## Output Format

Use this structure:

1. `Findings`
2. `Open Questions`
3. `Residual Risk`

Under `Findings`, order items from highest severity to lowest severity.

## Rules

- Prefer concrete failure modes over generic quality commentary.
- If the diff is large, focus first on externally observable behavior and risky state changes.
- If review confidence is reduced by missing context or missing tests, state that clearly.
