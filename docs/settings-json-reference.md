# Settings Reference (`settings.json`)

Reference for all YesMem-relevant settings in `~/.claude/settings.json` (Claude Code configuration).

> **Note:** `settings.json` is Claude Code's own configuration file. The entries documented here are those YesMem requires for operation or sets automatically via `yesmem install`.

---

## mcpServers — MCP Server Registration

Registers the YesMem MCP server with Claude Code. Without this entry, no YesMem tools are available.

```json
{
  "mcpServers": {
    "yesmem": {
      "command": "~/.local/bin/yesmem",
      "args": ["mcp"]
    }
  }
}
```

| Field | Description |
|-------|-------------|
| `command` | Absolute path to the yesmem binary. |
| `args` | MCP mode arguments (always `["mcp"]`). |

Set automatically by `yesmem install` and updated by `yesmem update` via the MCP registration helpers in `internal/setup` (there is no `EnsureMCPRegistration()` function; the merge lives in the settings and opencode-settings writers).

---

## env — Environment Variables

Inherited by every Claude Code session.

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:9099",
    "CLAUDE_CODE_REPL": "true"
  }
}
```

| Variable | Value | Description |
|----------|-------|-------------|
| `ANTHROPIC_BASE_URL` | `http://localhost:9099` | Routes Claude Code through the YesMem proxy instead of directly to the Anthropic API. **Without this entry the proxy does not function.** |
| `CLAUDE_CODE_REPL` | `true` | Enables the REPL environment (required for some features). |

---

## hooks — Hook Registration

YesMem registers hooks for various Claude Code lifecycle events.

### SessionStart

Fires at session start (`startup`, `resume`, `clear`, `compact`).

```json
{
  "hooks": {
    "SessionStart": [{
      "hooks": [{
        "command": "~/.local/bin/yesmem briefing-hook",
        "type": "command"
      }],
      "matcher": "startup|resume|clear|compact"
    }]
  }
}
```

| Field | Description |
|-------|-------------|
| `command` | `yesmem briefing-hook` — generates the session briefing and injects it into context. |
| `matcher` | Regex for event types that trigger this hook. |

### SessionEnd

Fires at session end. Registered **without a matcher** on purpose: `compact` is not one of the SessionEnd reasons, so it must not be baked into a matcher. The documented reasons are `clear`, `resume`, `logout`, `prompt_input_exit` and `other`; `RunSessionEnd` selects the ones yesmem actually tracks.

```json
{
  "SessionEnd": [{
    "hooks": [{
      "command": "~/.local/bin/yesmem session-end",
      "type": "command"
    }]
  }]
}
```

| Field | Description |
|-------|-------------|
| `command` | `yesmem session-end` — closes the session and triggers the extraction pipeline. |

### PreToolUse

Fires before every tool call.

```json
{
  "PreToolUse": [{
    "hooks": [{
      "command": "~/.local/bin/yesmem hook-check",
      "type": "command"
    }],
    "matcher": ".*"
  }]
}
```

| Field | Description |
|-------|-------------|
| `command` | `yesmem hook-check` — code-navigation hook (blocks `Bash(find)`, `Bash(grep)`, `Bash(cat)` and suggests MCP code tools). |
| `matcher` | `.*` — applies to all tools. |

### PostToolUse

Fires after a successful tool call.

```json
{
  "PostToolUse": [{
    "hooks": [{
      "command": "~/.local/bin/yesmem hook-resolve",
      "type": "command"
    }],
    "matcher": "Bash",
    "if": "Bash(git *)"
  }]
}
```

| Field | Description |
|-------|-------------|
| `command` | `yesmem hook-resolve` — after git operations, checks if open tasks can be resolved. |
| `matcher` | `Bash` — only Bash tool calls. |
| `if` | `Bash(git *)` — additional filter, only git commands. |

### PostToolUseFailure

Fires after a failed tool call.

```json
{
  "PostToolUseFailure": [{
    "hooks": [{
      "command": "~/.local/bin/yesmem hook-failure",
      "type": "command"
    }],
    "matcher": ".*"
  }]
}
```

| Field | Description |
|-------|-------------|
| `command` | `yesmem hook-failure` — processes failed tool calls for learning/analysis. |

### Stop

Fires on session exit (after `/exit`).

```json
{
  "Stop": [{
    "hooks": [{
      "command": "~/.claude/hooks/notify-context.sh",
      "type": "command"
    }]
  }]
}
```

| Field | Description |
|-------|-------------|
| `command` | Notification script (optional, not YesMem-specific). |

### Notification

Fires on certain events (context warnings, etc.).

```json
{
  "Notification": [{
    "hooks": [{
      "command": "~/.claude/hooks/notify-context.sh",
      "type": "command"
    }],
    "matcher": ""
  }]
}
```

### UserPromptSubmit

Carries the idle tick that turns a prompt into a yesmem usage reminder when the session has been idle:

```json
{
  "UserPromptSubmit": [{
    "hooks": [{
      "command": "~/.local/bin/yesmem idle-tick",
      "type": "command"
    }]
  }]
}
```

---

## permissions — Tool Permissions

YesMem MCP tools require `allow` entries in Claude Code permissions. These are set automatically by `yesmem install`.

```json
{
  "permissions": {
    "allow": [
      "mcp__yesmem__search",
      "mcp__yesmem__remember",
      "mcp__yesmem__pin",
      "mcp__yesmem__unpin",
      "mcp__yesmem__get_pins",
      "mcp__yesmem__send_to",
      "mcp__yesmem__broadcast",
      "mcp__yesmem__deep_search",
      "mcp__yesmem__get_session",
      "mcp__yesmem__list_projects",
      "mcp__yesmem__project_summary",
      "mcp__yesmem__get_learnings",
      "mcp__yesmem__query_facts",
      "mcp__yesmem__related_to_file",
      "mcp__yesmem__get_coverage",
      "mcp__yesmem__get_project_profile",
      "mcp__yesmem__get_self_feedback",
      "mcp__yesmem__set_persona",
      "mcp__yesmem__get_persona",
      "mcp__yesmem__resolve",
      "mcp__yesmem__resolve_by_text",
      "mcp__yesmem__quarantine_session",
      "mcp__yesmem__skip_indexing",
      "mcp__yesmem__set_plan",
      "mcp__yesmem__update_plan",
      "mcp__yesmem__get_plan",
      "mcp__yesmem__complete_plan",
      "mcp__yesmem__hybrid_search",
      "mcp__yesmem__get_compacted_stubs",
      "mcp__yesmem__expand_context",
      "mcp__yesmem__set_config",
      "mcp__yesmem__get_config",
      "mcp__yesmem__docs_search",
      "mcp__yesmem__list_docs",
      "mcp__yesmem__ingest_docs",
      "mcp__yesmem__remove_docs",
      "mcp__yesmem__scratchpad_write",
      "mcp__yesmem__scratchpad_read",
      "mcp__yesmem__scratchpad_list",
      "mcp__yesmem__scratchpad_delete",
      "mcp__yesmem__spawn_agent",
      "mcp__yesmem__relay_agent",
      "mcp__yesmem__stop_agent",
      "mcp__yesmem__resume_agent",
      "mcp__yesmem__list_agents",
      "mcp__yesmem__get_agent",
      "mcp__yesmem__update_agent_status",
      "mcp__yesmem__whoami",
      "mcp__yesmem__stop_all_agents",
      "mcp__yesmem__activate_cap",
      "mcp__yesmem__deactivate_cap",
      "mcp__yesmem__save_cap",
      "mcp__yesmem__get_caps",
      "mcp__yesmem__register_caps",
      "mcp__yesmem__cap_store",
      "mcp__yesmem__get_code_context",
      "mcp__yesmem__get_code_snippet",
      "mcp__yesmem__get_dependency_map",
      "mcp__yesmem__get_file_index",
      "mcp__yesmem__get_file_symbols",
      "mcp__yesmem__graph_traverse",
      "mcp__yesmem__search_code",
      "mcp__yesmem__search_code_index",
      "mcp__yesmem__dismiss_code_nav",
      "mcp__yesmem__dismiss_repl_pattern",
      "mcp__yesmem__schedule",
      "mcp__yesmem__relate_learnings"
    ],
    "defaultMode": "auto"
  }
}
```

| Field | Description |
|-------|-------------|
| `allow` | List of all permitted YesMem MCP tools, written as `mcp__yesmem__<tool>`. |
| `defaultMode` | `auto` is the **classifier** mode: Claude Code decides per tool call instead of prompting for everything. Tools not listed in `allow` are not silently permitted — they go through that decision. |

---

## statusLine — Status Line

Displays YesMem status info in the terminal status line.

```json
{
  "statusLine": {
    "command": "~/.local/bin/yesmem statusline",
    "refreshInterval": 2,
    "type": "command"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `command` | string | `yesmem statusline` — outputs cache TTL, token stats, collapsing savings, keepalive timeline, session ID. |
| `refreshInterval` | int | Refresh interval in seconds. |
| `type` | string | `command` — command output is displayed in the status line. |

---

## sandbox — Filesystem Permissions

Allows write access within sandbox environments (e.g. agent spawns).

```json
{
  "sandbox": {
    "filesystem": {
      "allowWrite": [
        "/tmp",
        "<your-project-path>",
        "<your-ansible-path>"
      ]
    }
  }
}
```

| Field | Description |
|-------|-------------|
| `filesystem.allowWrite` | List of directories writable from within the sandbox. |

---

## Other Relevant Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `autoCompactEnabled` | bool | `false` | Automatic context compaction (should be `false` since the YesMem proxy manages compaction). |
| `cleanupPeriodDays` | int | `30` | Days until session data is automatically cleaned up. Set high (`99999`) when using the proxy. |
| `enabledPlugins` | object | `{}` | Enabled Claude Code plugins. YesMem needs `superpowers@claude-plugins-official: true` for plan/skill features. |
| `tui` | string | `"default"` | Terminal UI mode. |
| `includeCoAuthoredBy` | bool | `false` | Co-authored-by in commits. |
| `model` | string | — | Default model (e.g. `"opus[1m]"`). |
| `skipAutoPermissionPrompt` | bool | `false` | Skip automatic permission prompts. |

---

> **See also:** `config-reference.md` — all YesMem `config.yaml` settings.
