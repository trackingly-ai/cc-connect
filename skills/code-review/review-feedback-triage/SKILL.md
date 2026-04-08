---
name: review-feedback-triage
description: Handle incoming code review feedback rigorously. Use when review comments are unclear, questionable, conflicting, or need prioritization before implementation.
---

# Review Feedback Triage

Use this skill when code review feedback has already arrived and you need to decide what to do with it.

## Goals

1. Understand the actual request.
2. Separate valid issues from weak or unclear suggestions.
3. Clarify ambiguity before making changes.
4. Implement only after the feedback is technically grounded.

## Process

### 1. Restate the feedback precisely

- Summarize each review comment in plain technical terms.
- If multiple comments exist, turn them into a numbered list.

### 2. Classify each comment

Put each item into one of these buckets:

- `Valid and clear`
- `Valid but unclear`
- `Questionable`
- `Out of scope`
- `Duplicate of another comment`

### 3. Verify before changing code

For each item:

- Check the relevant code path.
- Look for reproducer conditions, tests, or concrete evidence.
- If the reviewer suggests a refactor, verify the claimed benefit.
- If the comment conflicts with existing architecture or product requirements, call that out explicitly.

### 4. Decide the response

- Implement immediately if the issue is valid and clear.
- Ask clarifying questions if the issue is valid but underspecified.
- Push back with technical reasoning if the suggestion is incorrect.
- Defer if the issue is real but belongs in a follow-up.

## Response Rules

- Avoid performative agreement.
- Prefer technical statements over social filler.
- If a reviewer is wrong, explain why with evidence.
- If a reviewer is right, move directly to the fix plan.

## Output Format

Produce:

1. A short triage table or numbered list of comments and dispositions.
2. Any clarifying questions that must be answered before editing.
3. A concrete implementation plan for the comments you accept.
