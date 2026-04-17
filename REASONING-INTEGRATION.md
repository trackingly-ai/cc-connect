# Reasoning Level Integration

This document defines how `cc-connect` should support runtime and config-driven
reasoning-depth selection across Codex, Claude Code, Gemini CLI, and Qoder.

The goal is:

- expose a stable user-facing control in chat
- preserve native behavior where an agent already has a first-class effort knob
- avoid inventing a fake abstraction where the underlying agent does not
  support it
- keep Feishu interactions card-first so users do not need to type fragile
  values manually

## Scope

This proposal covers:

- config file support
- runtime slash command support
- Feishu card UX
- agent-specific adapter behavior
- test plan

This proposal does not cover:

- OpenCode / Cursor / iFlow / OpenCode Router integration
- provider-level reasoning controls outside the agent CLI

## Capability Matrix

### Codex

- Native model selection: yes
- Native reasoning-effort selection: yes
- Current local config key: `model_reasoning_effort`
- Recommended `cc-connect` runtime command: `/effort`

Supported effort levels:

- `low`
- `medium`
- `high`
- `xhigh`

Notes:

- Codex CLI help exposes `--model` directly.
- The local Codex config and session metadata show `model_reasoning_effort`
  and `reasoning_effort`.
- `cc-connect` should treat Codex as a first-class native reasoning-effort
  backend.

References:

- OpenAI GPT-5.2-Codex model docs: https://developers.openai.com/api/docs/models/gpt-5.2-codex

### Claude Code

- Native model selection: yes
- Native reasoning-effort selection: yes
- Native flag: `--effort`
- Native runtime command: `/effort`
- Native settings key: `effortLevel`

Supported levels:

- `auto`
- `low`
- `medium`
- `high`
- `max`

Notes:

- Claude Code already treats effort as a first-class session property.
- `max` is model-limited. `cc-connect` should still expose it and let Claude
  decide whether the active model supports it.

References:

- Claude Code model config docs: https://code.claude.com/docs/en/model-config
- Claude Code commands docs: https://code.claude.com/docs/en/commands
- Claude Code settings docs: https://code.claude.com/docs/en/settings

### Gemini CLI

- Native model selection: yes
- Native reasoning-effort selection: not as a simple top-level effort flag
- Native advanced control: `thinkingConfig.thinkingBudget`
- Recommended `cc-connect` runtime command: `/effort` with bridge-defined
  presets

Recommended preset levels:

- `off`
- `low`
- `medium`
- `high`

Recommended preset mapping:

- `off` -> `thinking_budget = 0`
- `low` -> `thinking_budget = 256`
- `medium` -> `thinking_budget = 1024`
- `high` -> `thinking_budget = 4096`

Notes:

- Gemini should be supported, but as a bridge-defined abstraction.
- `cc-connect` should also expose a raw numeric config path for advanced users:
  `thinking_budget`.
- When `thinking_budget` is explicitly configured, it should override preset
  `reasoning_level`.

References:

- Gemini CLI model selection docs: https://geminicli.com/docs/cli/model/
- Gemini CLI advanced model configuration docs: https://geminicli.com/docs/cli/generation-settings/

### Qoder

- Native model selection: yes
- Native reasoning-effort selection: no separate knob
- Model tier already encodes capability / reasoning depth

Native tiers:

- `lite`
- `efficient`
- `auto`
- `performance`
- `ultimate`

Notes:

- Qoder should not get a separate effort abstraction in v1.
- Users should continue using `/model`.
- If a user tries `/effort` on a Qoder project, `cc-connect` should reply that
  Qoder uses model tiers for reasoning depth and suggest `/model`.

References:

- Qoder model docs: https://docs.qoder.com/cli/model

## Unified User Experience

### Runtime command

Add a new slash command:

- `/effort`

Optional alias:

- `/reasoning`

Behavior:

- no argument:
  - show current effort / reasoning level
  - show available options
  - on Feishu, send an interactive card with one button per row
- with argument:
  - set the selected value
  - reset the running agent session
  - persist the new setting in memory for subsequent sessions in this project

Examples:

```text
/effort
/effort high
/reasoning medium
```

### Feishu card behavior

For Feishu:

- one option per row
- short, stable labels
- avoid mixed model and effort controls in the same card

Recommended button sets:

- Codex: `low`, `medium`, `high`, `xhigh`
- Claude: `auto`, `low`, `medium`, `high`, `max`
- Gemini: `off`, `low`, `medium`, `high`
- Qoder: no `/effort` card; user should use `/model`

## Config Design

### Generic config

Add a cross-agent option in `projects.agent.options`:

```toml
reasoning_level = "high"
```

This is the main cross-agent config knob.

### Gemini-specific advanced config

Also support:

```toml
thinking_budget = 1024
```

Rules:

- For Gemini only, `thinking_budget` overrides `reasoning_level`
- For other agents, `thinking_budget` is ignored

### Qoder behavior

For Qoder:

- `reasoning_level` should not be honored
- `cc-connect` should log a warning during agent construction if it is present
- docs should direct users to `model = "auto" | "performance" | "ultimate" | ...`

### Example config

#### Codex

```toml
[[projects]]
name = "codex"
  [projects.agent]
    type = "codex"
    [projects.agent.options]
      work_dir = "/Users/edward/Projects"
      model = "gpt-5.2-codex"
      reasoning_level = "high"
```

#### Claude Code

```toml
[[projects]]
name = "claude"
  [projects.agent]
    type = "claudecode"
    [projects.agent.options]
      work_dir = "/Users/edward/Projects"
      model = "sonnet"
      reasoning_level = "medium"
```

#### Gemini

```toml
[[projects]]
name = "gemini"
  [projects.agent]
    type = "gemini"
    [projects.agent.options]
      work_dir = "/Users/edward/Projects"
      model = "gemini-2.5-pro"
      reasoning_level = "high"
```

Or with explicit budget:

```toml
[[projects]]
name = "gemini"
  [projects.agent]
    type = "gemini"
    [projects.agent.options]
      work_dir = "/Users/edward/Projects"
      model = "gemini-2.5-pro"
      thinking_budget = 2048
```

#### Qoder

```toml
[[projects]]
name = "qoder"
  [projects.agent]
    type = "qoder"
    [projects.agent.options]
      work_dir = "/Users/edward/Projects"
      model = "performance"
```

## Core Interface Design

Add a new optional interface in `core/interfaces.go`:

```go
type ReasoningLevelOption struct {
    Key  string
    Name string
    Desc string
}

type ReasoningLevelSwitcher interface {
    SetReasoningLevel(level string)
    GetReasoningLevel() string
    AvailableReasoningLevels(ctx context.Context) []ReasoningLevelOption
}
```

This mirrors `ModelSwitcher` and `ModeSwitcher`.

## Engine Changes

### New command

In `core/engine.go`:

- register `/effort`
- optionally register `/reasoning` as an alias to `/effort`
- implement `cmdEffort(...)`

Behavior:

- if agent does not implement `ReasoningLevelSwitcher`:
  - reply with "This agent does not support independent reasoning level switching."
  - for Qoder, append "Use `/model` to choose a model tier instead."

### Session reset

Changing effort should mirror model switching:

- update the agent adapter field
- clear active session id
- clear interactive state
- clear local history for the active session
- persist session metadata

This avoids mixing old context produced under a different reasoning regime.

## Agent Adapter Changes

### Codex adapter

Files:

- `agent/codex/codex.go`
- `agent/codex/session.go`

Changes:

- add `reasoningLevel string` to `Agent`
- read `projects.agent.options.reasoning_level`
- implement `ReasoningLevelSwitcher`
- pass effort into `newCodexSession(...)`
- in session launch args, append:

```text
-c model_reasoning_effort="high"
```

Available levels:

- `low`
- `medium`
- `high`
- `xhigh`

Default:

- empty string means use Codex default

### Claude Code adapter

Files:

- `agent/claudecode/claudecode.go`
- `agent/claudecode/session.go`

Changes:

- add `reasoningLevel string` to `Agent`
- read `projects.agent.options.reasoning_level`
- implement `ReasoningLevelSwitcher`
- pass effort into session args as:

```text
--effort high
```

Available levels:

- `auto`
- `low`
- `medium`
- `high`
- `max`

Notes:

- `auto` should mean "do not pass `--effort`" or explicitly reset to default
- first implementation may normalize `auto` to empty string on launch

### Gemini adapter

Files:

- `agent/gemini/gemini.go`
- `agent/gemini/session.go`

Changes:

- add:
  - `reasoningLevel string`
  - `thinkingBudget *int`
- read both from `projects.agent.options`
- implement `ReasoningLevelSwitcher` using bridge-defined presets
- add session launch support for Gemini model config injection

Recommended v1 implementation:

- if `thinking_budget` is set:
  - write a transient Gemini config snippet for this session
  - set `thinkingConfig.thinkingBudget`
- else if `reasoning_level` is set:
  - map to the preset budget above

Notes:

- Gemini is the only agent in this design where the runtime effort abstraction
  is not a native 1:1 CLI switch.
- This should be documented clearly in help text and release notes.

### Qoder adapter

Files:

- `agent/qoder/qoder.go`

Changes:

- no `ReasoningLevelSwitcher`
- if `reasoning_level` is configured, log a warning that Qoder uses model tiers
  instead

## Help Text / i18n

Add:

- `/effort [name]`
- localized "current effort"
- localized "available effort levels"
- localized "agent does not support reasoning level switching"

For Qoder:

- specialized hint: "Qoder uses `/model` tiers instead of a separate effort control."

## Feishu UX Rules

For `/effort` cards:

- one option per row
- do not include long explanatory paragraphs
- current selection should be prefixed with `▶`

Examples:

- `▶ medium`
- `high`
- `xhigh`

## Testing Plan

### Core tests

Add tests for:

- `/effort` on Feishu returns one button per row
- `/effort 2` resolves against the displayed option list
- unsupported agents fall back to a clear error message
- Qoder path suggests `/model`

### Codex tests

Add tests for:

- `SetReasoningLevel("high")` updates agent state
- `StartSession` / `Send` include:

```text
-c model_reasoning_effort="high"
```

### Claude tests

Add tests for:

- `SetReasoningLevel("high")`
- launch args include:

```text
--effort high
```

### Gemini tests

Add tests for:

- preset mapping from `reasoning_level` to `thinking_budget`
- explicit `thinking_budget` overrides preset
- launch path receives the expected transient config / env

### Qoder tests

Add tests for:

- `reasoning_level` is ignored with a warning
- `/effort` reports unsupported
- `/model` remains the supported path

## Recommended Delivery Order

### Phase 1

Implement:

- Codex
- Claude Code
- `/effort` core command
- Feishu effort cards

This delivers the highest-confidence native support first.

### Phase 2

Implement:

- Gemini preset mapping
- Gemini explicit `thinking_budget`

### Phase 3

Implement:

- Qoder warning / unsupported messaging polish
- docs and help text cleanup

## Recommended First Release Behavior

For the first release, the user-facing contract should be:

- Codex: fully supported
- Claude Code: fully supported
- Gemini: supported with bridge-defined presets
- Qoder: use `/model`, no independent `/effort`

This keeps the abstraction honest while still covering all four agents in a
clear, usable way.
