---
name: review-packet
description: Prepare a structured handoff packet for a second reviewer or review agent. Use when another reviewer needs the essential repo, diff, scope, and risk context without re-reading the entire conversation.
---

# Review Packet

Prepare a compact but sufficient packet that another reviewer can use immediately.

## Include Only What Is Needed

The packet should contain:

- Repository path or review target
- Branch or commit under review
- Whether to review `working_tree`, `last_commit`, or `summary_only`
- Key changed files
- Short change summary
- The main review risks or questions

## Packet Construction

### 1. Decide the review target

- Use `working_tree` if relevant uncommitted changes matter.
- Use `last_commit` if the work is already committed and that is the intended review unit.
- Use `summary_only` only when code state is not necessary.

### 2. Compress the context

Include:

- What changed
- Why it changed
- What is risky
- What the second reviewer should pay attention to first

Exclude:

- Long execution logs
- Full diffs
- Unrelated files
- Conversational filler

### 3. Frame the review ask

The packet should direct the reviewer toward the real risk:

- behavioral correctness
- regression risk
- migration or compatibility concerns
- missing validation or test coverage
- security-sensitive edges

## Output Format

Return a structured packet with:

1. Task type
2. Review scope
3. Change summary
4. Repo path, branch, commit if applicable
5. File list
6. Review instructions

## Rules

- Optimize for reviewer startup speed.
- Do not include unrelated working-tree noise.
- Prefer exact paths and commit IDs over vague descriptions.
