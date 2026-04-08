# Per-Project Native Skills Integration

Status: draft design

## Validation Status

This document now distinguishes between:

- documented behavior confirmed from official product docs
- local behavior validated empirically on this machine with temporary test
  workspaces

The local validation described below used a minimal skill:

- `name: test-skill`
- `description: Use when the user says TRIGGER_SKILL_TEST. Reply exactly with SKILL_OK.`

and tested whether each CLI could discover and activate the skill when launched
from a temporary workspace.

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

There should be two distinct execution modes.

### Mode A: Projects With `skill_dirs`

If a project has non-empty effective `skill_dirs`, `cc-connect` should stop
using the repo `work_dir` as the agent's primary workspace.

Instead, it should create and manage a session-scoped workspace:

```text
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/
```

Inside that managed workspace, `cc-connect` should materialize the merged
effective skill set into the agent's native skill folder:

```text
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/.<agent>/skills/
```

or the agent-specific equivalent such as `.agents/skills`.

Then `cc-connect` should:

1. `cd` the agent process into this managed workspace
2. expose the real repo roots separately through the agent CLI's native
   "additional directories" mechanism where such a mechanism exists
3. merge all resolved project skill roots into one native materialized skill
   directory for that session

This is the isolation model.

### Mode B: Projects Without `skill_dirs`

If a project does not define any effective `skill_dirs`, keep the current
behavior:

1. use the configured `work_dir` directly as the agent workspace
2. launch the agent from that directory
3. do not create a managed session workspace

This preserves today's simpler behavior for ordinary projects that do not need
per-role native skill isolation.

### Why This Split Is Needed

Native skills are discovered from workspace-specific directories. If two
projects of the same agent type share one physical workspace, they also share
the same native skills view.

A managed workspace solves that:

- role-specific native skills become isolated per `cc-connect` project
- the real repo roots can still be made visible to the agent
- one agent binary can serve multiple roles with different native skills
- projects without native-skill needs stay on the old simpler path

### Materialization Strategy

- prefer symlinks where the agent explicitly documents or empirically supports
  them
- fall back to managed copies only where symlink behavior is undocumented or
  unsupported
- merge all resolved effective skill roots into one native target directory for
  the session
- the managed workspace should be treated as disposable session state, not the
  user's source-of-truth repo

## Why Per-Project Matters

The same base model can serve different roles:

- tester
- researcher
- reviewer
- release manager

Those roles should be represented as separate `cc-connect` projects, each with:

- its own project name
- its own execution workspace
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
  `skill_dirs`, `cc-connect` should materialize them into:

  ```text
  <managed-workspace>/.agents/skills/
  ```

- Each skill should appear as a directory:

  ```text
  <managed-workspace>/.agents/skills/<skill-name> -> /actual/source/<skill-name>
  ```

- The real repo roots should remain separate from native skill storage
- `--add-dir` can be used to widen writable scope for additional repo roots,
  but it does not replace native skill discovery in the main workspace

Recommended Codex mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Create/update symlinks under `<managed-workspace>/.agents/skills`
4. Remove stale symlinks that no longer belong to the current project
5. Launch Codex with:
   - `--cd <managed-workspace>`
   - zero or more `--add-dir <repo-root>` entries for real work roots when
     writable access is needed there
6. Start a new Codex session, or require a new session if Codex only discovers
   skills at startup

Local validation result:

- validated in a temporary workspace using `.agents/skills/test-skill/SKILL.md`
- `codex exec` explicitly reported that it was using `test-skill`
- Codex read the skill file from `.agents/skills/test-skill/SKILL.md`
- the run completed with `SKILL_OK`

Current confidence:

- documented and empirically validated
- empirically, Codex `--add-dir` extends writable sandbox scope
- empirically, Codex `--add-dir` does not make `.agents/skills` in the added
  directory participate in native skill discovery

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
  `skill_dirs`, `cc-connect` should materialize them into a managed
  workspace-native skill directory
- The preferred native target should be:

  ```text
  <managed-workspace>/.agents/skills/
  ```

  because the alias is explicitly documented and interoperable across agent
  tools

- Symlink-based materialization is preferred and aligns with Gemini's own
  documented `skills link` flow

Recommended Gemini mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Create/update symlinks under `<managed-workspace>/.agents/skills`
4. Remove stale symlinks previously managed by `cc-connect`
5. Launch Gemini from the managed workspace and use the Gemini CLI
   `--include-directories <repo-root>` flag for real work roots that must be
   visible to the agent
6. Start a new Gemini session, or require a session refresh if the running
   session has already completed its startup-time skill discovery

Gemini therefore belongs in the same first implementation batch as Codex.

Local validation result:

- validated in a temporary workspace with both `.agents/skills/test-skill` and
  `.gemini/skills/test-skill`
- Gemini reported a skill conflict and correctly preferred `.agents/skills`
  over `.gemini/skills`, matching the documented precedence
- Gemini then printed that it would activate `test-skill`
- the specific run did not complete because the backend returned a model
  capacity `429` error

Current confidence:

- documented and discovery path empirically validated
- full end-to-end activation was attempted but interrupted by upstream capacity,
  not by local skill discovery failure

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
  <managed-workspace>/.qoder/skills/
  ```

- Qoder should be included in the first native integration batch
- because the docs do not explicitly promise symlink support or an external
  overlay mechanism, the safest default is workspace-local materialization
- Qoder should treat the managed workspace as the last and authoritative
  `-w` workspace, because native skill discovery follows that workspace

Recommended Qoder mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Materialize them under `<managed-workspace>/.qoder/skills`
4. Launch Qoder with the managed workspace as the final `-w`
5. Prefer deterministic reconcile behavior and assume a session restart is
   required after changes, because the official docs describe startup-time
   discovery

Open Qoder-specific question:

- whether symlink-based materialization is fully supported in practice

Local validation result:

- validated in a temporary workspace using `.qoder/skills/test-skill/SKILL.md`
- Qoder completed the run with `SKILL_OK`
- an additional local test used a symlinked skill directory under
  `.qoder/skills/test-skill`
- the symlinked version also completed with `SKILL_OK`

Current confidence:

- documented native project skills are confirmed
- symlink behavior works empirically on this machine, although it is still not
  explicitly promised by the docs
- empirically, when multiple `-w` flags are present, Qoder loads native skills
  from the last workspace only
- empirically, Qoder can still read explicit absolute paths outside `-w`, so
  `-w` is not a hard file-access sandbox

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
  <managed-workspace>/.claude/skills/
  ```

- this keeps Claude aligned with the common "project-local native skills"
  strategy used across the supported agents
- Claude should be launched from the managed workspace and use `--add-dir` for
  real repo roots that must be available to the agent

Recommended Claude mechanism:

1. Compute effective project skill roots
2. Enumerate valid skill directories under those roots
3. Materialize them under `<managed-workspace>/.claude/skills`
4. Reconcile managed entries and remove stale ones
5. Launch Claude from the managed workspace and pass `--add-dir <repo-root>`
   for each real work root that should be visible
6. Prefer a fresh Claude session after changes unless live detection is being
   explicitly relied upon

Local validation result:

- validated in a temporary workspace using `.claude/skills/test-skill/SKILL.md`
- Claude completed the run with `SKILL_OK`
- a second local test used an external overlay directory containing
  `.claude/skills/test-skill/SKILL.md` and launched Claude with `--add-dir`
- the overlay-based run also completed with `SKILL_OK`

Current confidence:

- documented and empirically validated for both project-local skills and the
  `--add-dir` overlay path

## Materialization Model

For projects using Mode A (`skill_dirs` present):

```text
project.skill_dirs -> effective skill set -> managed workspace native skill directory
```

Proposed helper layout:

```text
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/.cc-connect/skills-manifest.json
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/.agents/skills/...
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/.claude/skills/...
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/.gemini/skills/...
<data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/.qoder/skills/...
```

The manifest should record:

- project name
- agent type
- managed workspace path
- session key hash
- skill fingerprint
- source roots
- materialized target path
- list of linked skills
- last reconcile time

This allows safe cleanup and update.

## Reconciliation Rules

When `cc-connect` starts or reloads config:

1. Resolve effective skill roots for each project
2. If the project has effective `skill_dirs` and the agent supports native
   skills:
   - reconcile the managed workspace target directory
3. If the project has no effective `skill_dirs`:
   - keep the existing direct `work_dir` behavior
4. If the project/agent pair does not support native skills:
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
- for Mode A projects, the managed workspace should be created before session
  start and treated as part of session lifecycle state
- because the managed workspace path is keyed by `session_key_hash` and
  `skill_fingerprint`, the same chat session naturally reuses the same managed
  workspace while the effective native skill set remains unchanged
- if the effective native skill set changes, the `skill_fingerprint` changes
  too, so the next session startup naturally lands in a new managed workspace
  path

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
- real work roots passed through CLI flags should also be absolute
- managed workspaces should live under `<data_dir>` and be safe to recreate

## Phased Rollout

### Phase 1

Implement native materialization for:

- Codex
- Claude
- Gemini
- Qoder

Use the same high-level workflow for all four:

1. resolve project skill roots
2. create a managed session workspace when the project has effective
   `skill_dirs`
3. enumerate valid skills
4. reconcile the agent's native skills folder inside that managed workspace
5. pass real work roots separately through agent-specific CLI flags where
   supported
6. refresh or recreate the native session when needed

### Phase 2

Add session invalidation/restart behavior when native skill mappings change.

### Phase 3

Optionally add status/diagnostics:

- `/skills status`
- log materialized roots
- log linked skills per project

## Recommended First Implementation

Start with the common managed-workspace materialization framework for projects
that define native `skill_dirs`, while keeping direct `work_dir` launch for
projects that do not.

Common framework:

1. Detect projects where effective skill roots are non-empty
2. Allocate a managed workspace:

   ```text
   <data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/
   ```

3. Resolve the agent-specific target directory inside that managed workspace
4. Reconcile materialized skills into that target
5. Launch the agent from the managed workspace
6. Pass real work roots separately through agent-specific CLI flags when
   supported
7. Reconcile on startup and on config reload
8. Restart or recreate the native session if the materialized skill set changes

Initial target mapping:

- Codex -> `<managed-workspace>/.agents/skills`
- Claude -> `<managed-workspace>/.claude/skills`
- Gemini -> `<managed-workspace>/.agents/skills`
- Qoder -> `<managed-workspace>/.qoder/skills`

This yields one reusable implementation with agent-specific target path
selection rather than four separate integration models.

Direct-workdir fallback:

- if a project has no effective `skill_dirs`, keep the existing behavior
- launch directly from `work_dir`
- do not allocate a managed session workspace
- do not try to split "skill workspace" from "repo workspace"

Empirical summary so far:

- Codex project-local skills: works
- Claude project-local skills: works
- Claude `--add-dir` overlay skills: works
- Gemini project-local discovery and activation start: works, final response
  blocked by upstream `429`
- Qoder project-local skills: works
- Qoder symlinked project-local skills: works on this machine
- Qoder with multiple `-w`: native skills load from the last workspace only
- Qoder outside-path read: works when given an explicit absolute path
- Codex `--add-dir`: extends writable sandbox scope
- Codex `--add-dir`: does not load native skills from the added directory

## Skill Fingerprint

The `skill_fingerprint` should describe only the effective native skill set
itself. It should not include absolute source paths, agent type, native target
path, or other runtime launch parameters.

Recommended definition:

```text
workspace_path =
  <data_dir>/workspaces/<project-name>/<session_key_hash>/<skill_fingerprint>/
```

```text
skill_fingerprint =
  md5(
    normalized list of:
      rel
      name
      md5(SKILL.md)
    sorted by name, then rel
  )
```

Normalized skill entry shape:

```text
skill[0].rel=flaky-pytest
skill[0].name=flaky-pytest
skill[0].skill_md_md5=aaa...

skill[1].rel=regression-check
skill[1].name=regression-check
skill[1].skill_md_md5=bbb...
```

Notes:

- `rel` is the skill path relative to its logical skill-set root, not an
  absolute filesystem path
- `name` is the resolved native skill name exposed to the agent
- entries should be sorted by `name`, then `rel`, before hashing
- the final `skill_fingerprint` can use md5 because this is a deterministic
  cache/workspace key rather than a security boundary

## Open Questions

- Does Qoder officially support symlinked skill directories?
- Should `cc-connect` expose a diagnostic command for native skill mapping
  status?
- For Gemini, should `cc-connect` pass one `--include-directories` per work root
  or consolidate to a smaller set such as `~/Projects`?

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
