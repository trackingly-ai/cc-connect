---
name: security-review
description: Run a focused security review on code changes. Use when changes touch auth, sessions, secrets, permissions, input handling, APIs, database writes, shell execution, or security-sensitive infrastructure.
---

# Security Review

Use this skill when the change has a real security surface. This is not a general style review.

## Trigger Areas

Run this skill when changes affect any of these:

- Authentication or authorization
- Session or token handling
- Secrets, credentials, or key management
- Request validation or user input parsing
- Database queries or migrations
- File system access
- Shell or process execution
- Public APIs, webhooks, or external integrations

## Review Checklist

### 1. Input handling

- Is untrusted input validated?
- Are dangerous defaults rejected?
- Is user-controlled data passed into queries, templates, commands, or file paths?

### 2. Access control

- Are authorization checks present and correctly placed?
- Can the caller access data or actions they should not?
- Are object-level permission checks missing?

### 3. Secret exposure

- Are secrets committed, logged, echoed, or returned?
- Are sensitive values masked in errors and telemetry?

### 4. Injection and execution risk

- SQL injection
- Command injection
- Path traversal
- Template or serialization abuse

### 5. Session and token safety

- Are tokens validated correctly?
- Are expiration, rotation, or revocation assumptions safe?
- Is the session boundary clear and enforceable?

### 6. Operational security

- Are defaults safe in production?
- Can misconfiguration quietly disable protections?
- Is there a secure fallback if an external dependency fails?

## Output Format

If issues exist, report:

- Severity
- Attack or abuse scenario
- Impact
- Recommended fix

If no issues are found, explicitly say that the pass found no concrete security bugs, and mention any limits such as not having runtime config or deployment context.

## Rules

- Focus on realistic abuse cases.
- Do not label everything as security-critical.
- Prefer specific exploit paths over vague "might be insecure" wording.
