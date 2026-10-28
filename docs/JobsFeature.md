# Scheduled Jobs

YesMem includes a built-in job scheduler that runs recurring or one-shot tasks directly from the daemon. Jobs support three execution modes and integrate tightly with capabilities, the REPL, and the memory system.

## Job Modes

| Mode | Execution | Cost | Use Case |
|------|-----------|------|----------|
| `agent` | Spawns a full Claude Code session | High (LLM tokens) | Complex multi-step tasks |
| `headless` | Single LLM call via daemon | Medium | Quick analysis, summaries |
| `bash` | Shell command in sandboxed environment | Zero (no LLM) | Monitoring, data collection, health checks |

Bash-mode is the default for automated operational tasks. It runs shell commands without any LLM involvement, making it effectively free. LLM usage only occurs on failure when auto-correct is enabled.

## MCP Interface

All job management happens through the `schedule` MCP tool:

```
mcp__yesmem__schedule({action: "create", ...})
mcp__yesmem__schedule({action: "list"})
mcp__yesmem__schedule({action: "delete", id: "job-id"})
mcp__yesmem__schedule({action: "run", id: "job-id"})
```

### Creating a Job

```javascript
await mcp__yesmem__schedule({
  action:       "create",
  name:         "proxy-health-check",
  cron:         "*/15 * * * *",        // every 15 minutes
  prompt:       "curl -s http://localhost:8484/health",
  mode:         "bash",
  cap_name:     "proxy_health",        // optional: reference a saved capability
  auto_correct: true,                  // LLM rewrites script on failure
  allowed_ports: "80,443,8484",        // network ports the sandbox allows
  recurring:    true                   // false = one-shot, auto-deletes after firing
})
```

### Parameters

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `name` | string | required | Human-readable job name |
| `cron` | string | required | 5-field cron expression (minute hour dom month dow) |
| `prompt` | string | required | The command (bash mode) or prompt (agent/headless mode) |
| `mode` | string | `"agent"` | Execution mode: `agent`, `headless`, or `bash` |
| `recurring` | bool | `true` | Repeating job vs. one-shot |
| `enabled` | bool | `true` | Whether the job fires on schedule |
| `cap_name` | string | `""` | Name of a saved capability whose `handler_bash` to execute |
| `auto_correct` | bool | `true` | On bash failure, invoke LLM to analyze and rewrite the script |
| `allowed_ports` | string | `"80,443"` | Comma-separated list of network ports the sandbox permits |

## Capability Integration

Jobs can reference saved capabilities by name via `cap_name`. When a bash-mode job fires:

1. The scheduler looks up the capability by name in the learning database
2. Extracts the `handler_bash` field from the capability definition
3. Executes that handler in the sandbox

This means you can build, test, and save a capability in a REPL session, then schedule it to run automatically:

```javascript
// Step 1: Build and test a capability
await mcp__yesmem__save_cap({
  name: "proxy_health",
  description: "Check proxy and daemon health metrics",
  handler_bash: "curl -sf http://localhost:8484/health | jq .",
  tested: true,
  tags: "monitoring,health"
})

// Step 2: Schedule it
await mcp__yesmem__schedule({
  action: "create",
  name: "proxy-health-check",
  cron: "*/15 * * * *",
  prompt: "",
  mode: "bash",
  cap_name: "proxy_health",
  allowed_ports: "80,443,8484"
})
```

Capabilities can also be activated in REPL sessions via `activate_cap()` and executed interactively before being scheduled.

## Sandbox (Bash Mode)

Bash-mode jobs run inside an [ai-jail](https://github.com/akitaonrails/ai-jail) sandbox that enforces:

- **Read-only filesystem** except `/tmp` (writable workspace)
- **Network restrictions**: only configured ports are reachable (default: 80, 443)
- **Process isolation**: Linux namespaces + seccomp-bpf filtering
- **No persistent side effects**: each run starts from a clean state

### Sandbox Setup

The `ai-jail` binary is automatically downloaded from GitHub on first use and cached at `~/.local/bin/ai-jail`. If the binary is unavailable, the run **fails closed**: every sandboxed call site constructs its sandbox with `FallbackUnsandboxed: false`, so the job returns an error instead of running unsandboxed.

### Sandbox Construction

```go
sandbox := NewSandbox(SandboxConfig{
    AllowedPorts: []int{80, 443, 8484},
})
output, exitCode, err := sandbox.Run(command)
```

The sandbox passes `allowed_ports` to ai-jail's network filter, restricting outbound connections to only those ports. This allows jobs to reach specific local services (e.g., port 8484 for the proxy health endpoint) while blocking everything else.

## Auto-Correct on Failure

When a bash job fails (non-zero exit code) and `auto_correct` is enabled, the error processing pipeline activates:

### Flow

```
Job fails (exit_code != 0)
    │
    ▼
processBashJobErrors()          ← daemon tick polls for unprocessed errors
    │
    ├─ cap_name set? ──yes──▶ autoCorrectBashCap()
    │                             │
    │                             ├─ Resolve the cap row and the failing script
    │                             ├─ Send the failure excerpt (failing command,
    │                             │  error, output) + cap metadata — not the script
    │                             ├─ Receive a REPLACE block or UNFIXABLE
    │                             ├─ Apply the block to the live script body
    │                             ├─ Verify in the sandbox, then save_cap
    │                             └─ Broadcast result to active sessions
    │
    └─ no cap_name ──────────▶ diagnoseBashError()
                                  │
                                  ├─ Send error context to Claude Sonnet
                                  ├─ Receive diagnosis + suggested fix
                                  └─ Broadcast diagnosis to active sessions
```

### What Sonnet Receives

The auto-correct prompt includes:
- The failing command, identified from the error text (withheld when the line
  carries a redacted literal)
- The exit code
- stdout/stderr output (truncated to ~2000 chars, UTF-8 safe, redacted)
- The cap metadata: name, description, the script names, and the script kind

The script body itself is deliberately absent: the pass may save its result into
a live cap, so the body stays on the daemon side.

### What Sonnet Returns

- One `REPLACE` block: the text to replace plus its replacement, or
  `UNFIXABLE: <reason>`
- The block is applied to the live body only if it matches exactly once. No
  match, several matches, more than one block, or a replacement carrying a
  redaction marker aborts the pass with the reason and writes nothing.
- An applied fix is classified: a minimal change is verified in the sandbox and
  saved to the capability definition, a substantial one is staged as a
  `cap_proposed` row for review and never executed.

### Error Storage

Every bash job run is recorded in the `bash_job_runs` table:

| Column | Type | Description |
|--------|------|-------------|
| `id` | INTEGER | Auto-increment primary key |
| `job_id` | TEXT | Foreign key to `scheduled_jobs.id` |
| `job_name` | TEXT | Job name at time of execution |
| `cap_name` | TEXT | Capability name (if any) |
| `command` | TEXT | The exact command that was executed |
| `status` | TEXT | `"ok"` or `"error"` |
| `exit_code` | INTEGER | Process exit code |
| `output` | TEXT | stdout + stderr (truncated) |
| `error_msg` | TEXT | Error message (if any) |
| `processed` | BOOLEAN | Whether auto-correct has handled this error |
| `created_at` | TIMESTAMP | When the run occurred |

`bash_job_runs` lives in **`yesmem.db`**, not in a cap-owned table, so `cap_store` cannot reach it — a `cap_store({table: "bash_job_runs"})` call resolves against the cap's own namespace in `caps.db` and finds nothing. Use the storage API or read the daemon's `get_job`/run output instead:

```go
runs, err := store.GetBashJobRuns(jobID, 10)
errors, err := store.GetUnprocessedBashErrors(5)
store.MarkBashJobRunProcessed(runID)
```

## Scheduler Loop

The daemon runs a scheduler tick every second. On each tick:

1. Load all enabled jobs from SQLite
2. For each job, evaluate whether the cron expression matches the current time
3. If a match: fire the job according to its mode
4. Update `last_run` timestamp
5. For non-recurring jobs: delete after firing
6. Poll `bash_job_runs` for unprocessed errors and run auto-correct

The scheduler starts automatically when the daemon boots and stops on shutdown.

## Database Schema

### `scheduled_jobs` Table

```sql
CREATE TABLE IF NOT EXISTS scheduled_jobs (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    cron TEXT NOT NULL,
    prompt TEXT NOT NULL DEFAULT '',
    enabled INTEGER NOT NULL DEFAULT 1,
    recurring INTEGER NOT NULL DEFAULT 1,
    mode TEXT NOT NULL DEFAULT 'agent',
    cap_name TEXT NOT NULL DEFAULT '',
    auto_correct INTEGER NOT NULL DEFAULT 1,
    allowed_ports TEXT NOT NULL DEFAULT '80,443',
    last_run TEXT,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
)
```

### `bash_job_runs` Table

```sql
CREATE TABLE IF NOT EXISTS bash_job_runs (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id TEXT NOT NULL,
    job_name TEXT NOT NULL DEFAULT '',
    cap_name TEXT NOT NULL DEFAULT '',
    command TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'ok',
    exit_code INTEGER NOT NULL DEFAULT 0,
    output TEXT NOT NULL DEFAULT '',
    error_msg TEXT NOT NULL DEFAULT '',
    processed INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL DEFAULT (datetime('now'))
)
```

## Source Files

| File | Responsibility |
|------|---------------|
| `internal/daemon/scheduler.go` | Scheduler loop, cron evaluation, job firing |
| `internal/daemon/handler_scheduler.go` | MCP handler: create, list, delete, run |
| `internal/daemon/sandbox.go` | Sandbox construction, ai-jail invocation |
| `internal/daemon/sandbox_download.go` | ai-jail binary download from GitHub releases |
| `internal/daemon/bash_error_handler.go` | Auto-correct pipeline, Sonnet integration |
| `internal/storage/scheduler.go` | SQLite CRUD for ScheduledJobRow and BashJobRun |
| `internal/storage/schema.go` | Table definitions and migrations |
| `internal/mcp/server.go` | MCP tool schema for the `schedule` tool |

## Adding New Fields

When adding a new field to scheduled jobs, it must be synchronized across 10+ locations. Missing any one of them compiles successfully but silently defaults the field. The full list, plus the extra sites that fields with their own semantics touch:

| Field kind | Additional sites beyond the 12 below |
|---|---|
| `sandbox` | script-level `Script.Sandbox` in `CAP.md`, resolution order (script > job > daemon default), `RunWithProfile` call sites |
| `interval_seconds` | cron/interval parsing and the one-shot cleanup |
| `model` | agent spawn argument construction |
| `script_name` | `findBashScript` resolution (mandatory when a cap declares several bash scripts) |
| `backend` | agent spawn plan per backend |

1. Schema `CREATE TABLE` definition (for fresh databases)
2. `ALTER TABLE` migration (for existing databases)
3. `ScheduledJobRow` struct in `internal/storage/scheduler.go`
4. `SaveScheduledJob` INSERT statement
5. `ListScheduledJobs` SELECT + Scan
6. `ScheduledJob` struct in `internal/daemon/scheduler.go`
7. Daemon loader (populating daemon struct from storage row)
8. `scheduleCreate` handler (parameter reading + job construction + DB row + response)
9. `scheduleList` handler (response assembly)
10. `fireJobBash` (sandbox construction)
11. `autoCorrectBashCap` (sandbox construction)
12. MCP tool schema in `internal/mcp/server.go`
