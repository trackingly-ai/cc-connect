---
name: review-scope
description: Determine the right scope for a code review before summarizing it. Use when you need to decide whether a review should stay local to changed files or expand to impacted callers, dependencies, and end-to-end flows.
---

# Review Scope

Use this skill before producing a review summary. The goal is to decide how wide the review must go.

## Classification

### Minor review

Use a focused review when most of these are true:

- Small number of files changed
- No public API or schema changes
- No new dependency or integration point
- Behavior is localized
- Risk is mostly inside the changed function or module

### Major review

Use a broad review when any of these are true:

- Multiple modules changed
- Public interfaces changed
- Behavior shifts across boundaries
- New dependency or persistent state introduced
- Security, auth, data flow, or migration path changed

## Output

Produce:

1. `Scope: MINOR` or `Scope: MAJOR`
2. Changed files to review directly
3. Additional impacted code that must be inspected
4. Key integration points that the reviewer must not miss

## Rules

- Do not assume changed files alone are enough for major changes.
- If auth, persistence, routing, or API contracts changed, default to a major review unless there is a strong reason not to.
