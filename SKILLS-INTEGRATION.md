# Per-Project Native Skills Integration

Status: draft design

## Goal

Allow different `cc-connect` projects to expose different native skill sets to
their underlying agents, even when multiple projects use the same agent type.

Example:

- `codex-tester` uses Codex with testing-focused skills
- `codex-researcher` uses Codex with research-focused skills
- `gemini-reviewer` uses Gemini with review-focused skills

The key requirement is to use each agent's native skill discovery mechanism
where possible, instead of relying only on bridge-side `/skills` expansion.

## Problem

Today `cc-connect` has a project-level `SkillRegistry`, and users can invoke
skills through `cc-connect` commands such as `/skills` and `/<skill-name>`.
That is useful, but it is bridge-driven:

- `cc-connect` discovers the skills
- `cc-connect` injects the `SKILL.md` body into the agent prompt when the user
  explicitly invokes a skill

This does not automatically make those skills visible to the agent's own native
skill system.

For some agents, especially Codex, Gemini, and Qoder, native skill discovery is
already part of the product. `cc-connect` should integrate with those native
mechanisms so each project can behave like a specialized agent lane with a
unique role and unique skills.

## Non-Goals

- Do not define skill contents directly in `config.toml`
- Do not create a single fake cross-agent skill protocol
- Do not force one directory layout on all agents when the agent has an
  official native mechanism
- Do not rely on undocumented native skill locations when a documented
  instruction or agent mechanism is safer

## Existing cc-connect Config

Projects already support:

```toml
[[projects]]
name = "codex-tester"
skill_dirs = [
  "/abs/path/to/tester-skills",
  "/abs/path/to/shared-testing-skills",
]
include_default_skill_dirs = false
```

Current semantics:

- If `skill_dirs` is empty, fall back to the agent's default `SkillDirs()`
- If `skill_dirs` is present, use only those directories by default
- If `include_default_skill_dirs = true`, append the agent defaults after the
  project-specific directories

This gives `cc-connect` a project-level source of truth for skill roots.

## High-Level Design

For agents with a documented native skill discovery mechanism, `cc-connect`
should materialize the configured project skill roots into the agent's native
workspace-scoped location before a session starts.

Preferred default strategy:

- use project-local native skill folders inside the workspace

Why this should be the default:

- all currently confirmed target agents support a project/workspace-level native
  skills location
- it gives one consistent mental model across agents
- it allows `cc-connect` to reuse most of the same reconcile logic and
  scaffolding
- it keeps role-specific skill sets attached to the project lane instead of a
  user-global location
- it avoids making first-batch support depend on agent-specific secondary
  features such as `--add-dir` or CLI-side skill management commands

Preferred approach:

1. Resolve the project's effective skill roots from config
2. Build a workspace-local materialized skill directory for the agent
3. Expose those skills using the agent's native discovery path
4. Restart or create a fresh agent session when required so the agent rescans

Preferred materialization mode:

- start with a shared workspace-local materialization model
- prefer symlinks where the agent explicitly documents or demonstrates support
- fall back to managed copies only where symlink behavior is undocumented or
  unsupported

Reasons:

- maximize shared code across agents
- keep the first implementation simple and predictable
- preserve a single "project role owns project-native skills" mental model
- avoid stale duplicated skill contents where symlinks are supported

## Why Per-Project Matters

The same base model can serve different roles:

- tester
- researcher
- reviewer
- release manager

Those roles should be represented as separate `cc-connect` projects, each with:

- its own project name
- its own work directory
- its own native skill set
- optionally its own instruction file or subagents

The project becomes the unit of role identity.

## Native Integration Strategy by Agent

### Codex

This is the clearest native integration path.

Codex officially documents:

- skills are directories with `SKILL.md`
- skills are available in the CLI, IDE extension, and app
- Codex scans `.agents/skills` from the current working directory up to the
  repository root
- it also scans user, admin, and system locations
- symlinked skill folders are supported

Official reference:

- https://developers.openai.com/codex/cli/agent-skills

Implication for `cc-connect`:

- When a project uses `agent.type = "codex"` and has project-specific
  `skill_dirs`, `cc-connect` can safely materialize them into:

  ```text
  <workspace>/.agents/skills/
  ```

- Each skill should appear as a directory:

  ```text
  <workspace>/.agents/skills/<skill-name> -> /actual/source/<skill-name>
  ```

- This should make the skill visible to Codex natively through `/skills`, `$`
  mention, or implicit invocation

Recommended Codex mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Create/update symlinks under `<workspace>/.agents/skills`
4. Remove stale symlinks that no longer belong to the current project
5. Start a new Codex session, or require a new session if Codex only discovers
   skills at startup

### Gemini

Gemini has an officially documented native skills system and should be treated
as a first-batch native integration target.

Officially documented behavior:

- workspace skills are discovered from `.gemini/skills/` or the
  `.agents/skills/` alias
- user skills are discovered from `~/.gemini/skills/` or the
  `~/.agents/skills/` alias
- `gemini skills link <path>` creates symlinks for local skill repositories
- at session start, Gemini injects the name and description of enabled skills
  into the system prompt
- when a skill is activated, Gemini adds the skill directory to allowed file
  paths and loads the skill instructions and resources

Official reference:

- https://geminicli.com/docs/cli/skills/

Implication for `cc-connect`:

- When a project uses `agent.type = "gemini"` and has project-specific
  `skill_dirs`, `cc-connect` can safely materialize them into a workspace-native
  skill directory
- The preferred native target should be:

  ```text
  <workspace>/.agents/skills/
  ```

  because the alias is explicitly documented and interoperable across agent
  tools

- Symlink-based materialization is preferred and aligns with Gemini's own
  documented `skills link` flow

Recommended Gemini mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Create/update symlinks under `<workspace>/.agents/skills`
4. Remove stale symlinks previously managed by `cc-connect`
5. Start a new Gemini session, or require a session refresh if the running
   session has already completed its startup-time skill discovery

Gemini therefore belongs in the same first implementation batch as Codex.

### Qoder

Qoder documents official native project-level and user-level skills and should
be treated as a native integration target.

Officially documented behavior:

- user skills live at `~/.qoder/skills/{skill-name}/SKILL.md`
- project skills live at `.qoder/skills/{skill-name}/SKILL.md`
- project skills override user skills when names conflict
- at startup, Qoder loads each skill's `name` and `description`
- when a skill is selected, Qoder loads the full `SKILL.md`
- updates take effect when Qoder CLI is started again

Official reference:

- https://docs.qoder.com/cli/Skills

Implication for `cc-connect`:

- When a project uses `agent.type = "qoder"` and has project-specific
  `skill_dirs`, `cc-connect` should materialize them into:

  ```text
  <workspace>/.qoder/skills/
  ```

- Qoder should be included in the first native integration batch
- because the docs do not explicitly promise symlink support or an external
  overlay mechanism, the safest default is workspace-local materialization

Recommended Qoder mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Materialize them under `<workspace>/.qoder/skills`
4. Prefer deterministic reconcile behavior and assume a session restart is
   required after changes, because the official docs describe startup-time
   discovery

Open Qoder-specific question:

- whether symlink-based materialization is fully supported in practice

### Claude Code

Claude Code documents official native skills support and should be treated as a
native integration target.

Officially documented behavior:

- personal skills live at `~/.claude/skills/<skill-name>/SKILL.md`
- project skills live at `.claude/skills/<skill-name>/SKILL.md`
- plugin skills are also supported
- nested `.claude/skills` directories are discovered when working in
  subdirectories
- skills are loaded automatically when relevant or invoked directly as
  `/skill-name`
- `.claude/commands/` remains supported, but skills are the preferred format
- `--add-dir` can load `.claude/skills/` from additional directories as a
  documented exception

Official references:

- https://code.claude.com/docs/en/skills
- https://docs.anthropic.com/en/docs/claude-code/settings
- https://docs.anthropic.com/en/docs/claude-code/sub-agents

Implication for `cc-connect`:

- Claude belongs in the first native integration batch
- the default integration path should still be the shared project-local model:

  ```text
  <workspace>/.claude/skills/
  ```

- this keeps Claude aligned with the common "project-local native skills"
  strategy used across the supported agents
- `--add-dir` remains a useful future optimization for overlay-based skill sets,
  but it should not be the primary first-batch design because a project-local
  folder model is simpler and shared across agents

Recommended Claude mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Materialize them under `<workspace>/.claude/skills`
4. Reconcile managed entries and remove stale ones
5. Prefer a fresh Claude session after changes unless live detection is being
   explicitly relied upon

## Materialization Model

For agents with a documented native skills path:

```text
project.skill_dirs -> effective skill set -> workspace native skill directory
```

Proposed helper layout:

```text
<workspace>/.cc-connect/skills-manifest.json
<workspace>/.agents/skills/...
<workspace>/.claude/skills/...
<workspace>/.gemini/skills/...
<workspace>/.qoder/skills/...
```

The manifest should record:

- project name
- agent type
- source roots
- materialized target path
- list of linked skills
- last reconcile time

This allows safe cleanup and update.

## Reconciliation Rules

When `cc-connect` starts or reloads config:

1. Resolve effective skill roots for each project
2. If the project/agent pair supports native skills:
   - reconcile the workspace target directory
3. If the project/agent pair does not support native skills:
   - do nothing at the filesystem layer
   - continue using bridge-level `/skills`

When reconciling:

- create target directory if missing
- create symlink for each discovered skill
- overwrite broken or stale symlinks
- remove symlinks previously managed by `cc-connect` but no longer desired
- never delete user-managed files outside the managed manifest set

## Discovery and Precedence

Project-specific skill roots should remain the source of truth from
`cc-connect` config.

Recommended precedence for native materialization:

1. project `skill_dirs`
2. optional default skill dirs if `include_default_skill_dirs = true`

This matches the current bridge-level behavior.

If two skills have the same name:

- let the agent's own resolution behavior apply where documented
- for materialization, avoid merging folders
- prefer deterministic target naming and log duplicates clearly

## Session Lifecycle

Some agents may only discover skills at startup or session creation.

Therefore:

- reconciliation should happen before `StartSession`
- config reload may need to invalidate current native sessions for the affected
  project if skill discovery is not live

Recommended policy:

- if skill roots change for a project, mark the current agent session as needing
  restart
- new sessions always see the updated native skill set
- for active sessions, prefer explicit session recreation rather than relying on
  undocumented live reload

## Safety Constraints

- `skill_dirs` must be absolute
- ignore missing directories with a warning, do not hard fail runtime startup
- only materialize directories that contain valid skill subdirectories
- never recursively copy arbitrary user directories into agent-native roots
- prefer symlink creation over copying
- if symlink creation fails on a platform, degrade explicitly and log it

## Phased Rollout

### Phase 1

Implement native materialization for:

- Codex
- Claude
- Gemini
- Qoder

Use the same high-level workflow for all four:

1. resolve project skill roots
2. enumerate valid skills
3. reconcile the agent's project-local native skills folder in the workspace
4. refresh or recreate the native session when needed

### Phase 2

Add session invalidation/restart behavior when native skill mappings change.

### Phase 3

Optionally add status/diagnostics:

- `/skills status`
- log materialized roots
- log linked skills per project

## Recommended First Implementation

Start with the common project-local materialization framework because all four
currently researched agents support a native project/workspace skills folder.

Common framework:

1. Detect projects where effective skill roots are non-empty
2. Resolve the agent-specific target directory inside the workspace
3. Reconcile materialized skills into that target
4. Reconcile on startup and on config reload
5. Restart or recreate the native session if the materialized skill set changes

Initial target mapping:

- Codex -> `<workspace>/.agents/skills`
- Claude -> `<workspace>/.claude/skills`
- Gemini -> `<workspace>/.agents/skills`
- Qoder -> `<workspace>/.qoder/skills`

This yields one reusable implementation with agent-specific target path
selection rather than four separate integration models.

## Open Questions

- Does Qoder officially support symlinked skill directories?
- Should `cc-connect` expose a diagnostic command for native skill mapping
  status?

## References

- Codex agent skills:
  https://developers.openai.com/codex/cli/agent-skills
- Claude skills:
  https://code.claude.com/docs/en/skills
- Gemini skills:
  https://geminicli.com/docs/cli/skills/
- Gemini system prompt:
  https://geminicli.com/docs/cli/system-prompt/
- Qoder skills:
  https://docs.qoder.com/cli/Skills
- Claude settings:
  https://docs.anthropic.com/en/docs/claude-code/settings
- Claude subagents:
  https://docs.anthropic.com/en/docs/claude-code/sub-agents
