---
name: review-summary
description: Produce a structured code review summary from a PR, diff, or branch. Use when the reviewer needs a concise but rigorous findings summary organized by severity and review area.
---

# Review Summary

Create a summary that helps another reviewer or approver understand the review result quickly.

## Inputs

- PR, branch diff, or commit range
- Optional issue/task requirements
- Optional focus areas such as performance, API safety, or security

## Process

### 1. Gather context

- Read the change description.
- Read the diff and changed-file list.
- If requirements exist, compare the change to those requirements.

### 2. Review in layers

At minimum, cover:

1. Architecture and interface impact
2. Implementation correctness and data flow
3. Testing, performance, and security

### 3. Summarize findings

Organize findings by severity:

- `Critical`
- `High`
- `Medium`
- `Low`

Each finding should include:

- File/path reference
- What is wrong
- Why it matters
- Suggested correction or next step

## Output Format

Use this structure:

1. `Overview`
2. `Requirement Alignment`
3. `Critical`
4. `High`
5. `Medium`
6. `Low`
7. `Overall Recommendation`

## Rules

- Prefer findings over praise.
- If there are no meaningful issues, say so explicitly.
- Do not hide uncertainty: mention missing context, missing tests, or unverified assumptions.
