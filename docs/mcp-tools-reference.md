## MCP Tools Reference (70 tools)

All tools registered in `internal/mcp/server.go:registerTools()`. Budget ceiling: `server_budget_test.go` enforces max 60K chars for tool descriptions (raised from 31K in 2026-07 to accommodate informative Use-Cue descriptions). No deprecated tool name forwarding exists — superseded names (e.g. `schedule_create`) were removed, not aliased.

Large results (>30K chars) include `_meta.anthropic/maxResultSizeChars` for truncation detection.

### Search & Retrieval
| Tool | Parameters | Description |
|------|-----------|-------------|
| `search` | `query`, `query_en?`, `project?`, `since?`, `before?`, `limit?` | Full-text search across conversation logs. Use when you need specific phrases, commands, or error messages from past sessions. Returns matching messages with session ID and timestamp. For semantic search use `hybrid_search`, for raw conversation content use `deep_search`. |
| `deep_search` | `query`, `query_en?`, `include_thinking?`, `include_commands?`, `project?`, `since?`, `before?`, `limit?` | Raw conversation history with ±3 messages of surrounding context. Use when you need the full untruncated content of past exchanges, including thinking blocks and command outputs. For semantic search across learnings use `hybrid_search`, for exact phrase matching use `search`. |
| `hybrid_search` | `query`, `project?`, `since?`, `before?`, `limit?` | Combined BM25 keyword + vector semantic search for learning retrieval. Use as your primary memory access tool — provides the best balance of precision and recall. For raw conversation content use `deep_search`, for exact phrase matching use `search`. |
| `docs_search` | `query`, `source?`, `section?`, `since?`, `before?`, `exact?`, `limit?`, `doc_type?` | Search indexed documentation by keyword or semantic meaning. Use before guessing API behavior, function signatures, or idiomatic patterns. Supports filtering by source, section, doc type, and exact BM25-only mode. For listing available sources use `list_docs`. |

### Learning Management
| Tool | Parameters | Description |
|------|-----------|-------------|
| `remember` | `text`, `category?`, `project?`, `model?`, `source?`, `origin?`, `supersedes?`, `entities?`, `actions?`, `trigger?`, `anticipated_queries?`, `context?`, `domain?`, `task_type?` | Save a lasting learning to persistent memory. Use after discovering a gotcha, making a decision, identifying a pattern, or receiving user feedback. Include structured metadata (entities, actions, trigger, anticipated_queries) for better retrieval. Model auto-resolved via param/YESMEM_MODEL_ID/proxy_state. |
| `get_learnings` | `id?`, `history?`, `category?`, `project?`, `since?`, `before?`, `limit?`, `task_type?` | Retrieve saved learnings by category, project, date range, or ID. Use to recall past decisions, gotchas, patterns, and user preferences. For full-text search across raw conversation logs use `search`, for semantic search use `hybrid_search`. Pass `history=true` with an ID to view the full version chain. |
| `resolve` | `learning_id`, `reason?` | Mark an unfinished task (learning with task_type) as resolved. Use after completing work tracked by a task-type learning. For text-based resolution without knowing the learning ID, use `resolve_by_text`. |
| `resolve_by_text` | `text`, `project?` | Find and resolve an unfinished task by searching its text content. Use after completing work when you don't have the learning ID at hand — matches against the learning's text field. Filters by project. |
| `quarantine_session` | `session_id` | Exclude all learnings from a session from search and injection. Use when a session produced bad data (loops, hallucinations, wrong approaches) that should not influence future work. |
| `skip_indexing` | `session_id` | Prevent a session from being indexed entirely. Use proactively when a session is unlikely to produce valuable learnings — saves processing and keeps the index clean. |

### Learning Metadata
| Tool | Parameters | Description |
|------|-----------|-------------|
| `query_facts` | `entity?`, `action?`, `keyword?`, `domain?`, `project?`, `category?`, `limit?` | Search learning metadata by entity, action, or keyword. Use for targeted lookups like "all learnings about proxy.go" or "every git push action". Hits the SQLite metadata table directly — faster than `hybrid_search` for very recent learnings that may not be vector-indexed yet. |
| `relate_learnings` | `learning_id_a`, `learning_id_b`, `relation_type` | Create a semantic relationship between two learnings. Use after saving related learnings to build a knowledge graph. Supported relations: `supports`, `contradicts`, `depends_on`, `relates_to`. |

### Session & Context
| Tool | Parameters | Description |
|------|-----------|-------------|
| `get_session` | `session_id`, `mode?`, `offset?`, `limit?` | Load a full session by ID. Use to explore past conversations in detail. Modes: `summary` (overview), `recent` (latest messages), `paginated` (chunked), `full` (complete). For searching across sessions use `search` or `deep_search`. |
| `get_compacted_stubs` | `session_id`, `from_idx?`, `to_idx?` | Retrieve compacted (archived) conversation stubs for a session. Use to zoom into previously collapsed conversation segments by index range. |
| `expand_context` | `query?`, `message_range?` | Expand archived conversation segments back into context. Use when a stub or collapsed block needs more detail. Filter by search query or message range. |
| `get_project_profile` | `project` | Get an auto-generated project portrait. Use for quick project overview — surfaces key files, technologies, session counts, and active areas. |
| `related_to_file` | `path` | Find all sessions that edited or referenced a specific file. Use when investigating a file's history across sessions or tracking down when a change was introduced. |
| `get_coverage` | `project` | Show which files were edited in a project across all sessions. Use for project overview and to discover hotspots. For per-file session history use `related_to_file`. |
| `list_projects` | — | List all known projects with session counts and last activity. Use for cross-project navigation and to discover active projects. |
| `project_summary` | `project`, `limit?` | Get a chronological project summary with recent sessions and activity. Use to understand what's been happening in a project recently. |

### Persona
| Tool | Parameters | Description |
|------|-----------|-------------|
| `set_persona` | `trait_key`, `value`, `dimension?` | Set a persona trait for the user profile. Use to record user preferences, expertise levels, or communication style. Dimension is auto-detected if empty. |
| `get_persona` | — | Get the current persona profile with all traits and confidence values. Use to recall user preferences, expertise, and communication style. |

### Self-Feedback
| Tool | Parameters | Description |
|------|-----------|-------------|
| `get_self_feedback` | `days?` | Retrieve recent corrections, confirmations, and feedback about your work. Use after returning from a break to catch up on what went wrong or right in recent sessions. Filters by number of days. |

### Configuration
| Tool | Parameters | Description |
|------|-----------|-------------|
| `set_config` | `key`, `value`, `session_id?` | Set a runtime configuration value. Use to adjust thresholds, tokens, or session parameters dynamically. Without session_id the change is global; with session_id it applies only to that session. |
| `get_config` | `key`, `session_id?` | Read a runtime configuration value. Use to check current settings like token thresholds. Checks session-specific overrides first, then falls back to global. |
| `pin` | `content`, `scope?`, `project?` | Pin a persistent instruction visible in every turn. Use for rules, constraints, or context that must survive context collapse. Scope: `session` (temporary) or `permanent`. Remove with `unpin`. |
| `unpin` | `id`, `scope?` | Remove a pinned instruction by ID. Use `get_pins` to list active pins and their IDs first. Scope must match the pin's scope (session or permanent). |
| `get_pins` | `project?` | List all active pinned instructions. Use to review current pins before adding or removing. Filter by project. |

### Agent Communication
| Tool | Parameters | Description |
|------|-----------|-------------|
| `send_to` | `target`, `content`, `msg_type?` | Send a message to another session. Use for inter-agent communication — target is a session ID. Message types: `command`, `response`, `ack`, `status`. |
| `whoami` | `project?` | Get your own session ID and agent metadata. Use at startup to discover your identity for `send_to` callbacks and to confirm your agent context (section, project, backend session ID). |
| `broadcast` | `content`, `project` | Send a message to all sessions in a project. Use for announcements, status updates, or coordination across all active sessions. |

### Documentation
| Tool | Parameters | Description |
|------|-----------|-------------|
| `ingest_docs` | `name`, `path`, `version?`, `project?`, `domain?`, `rules?`, `trigger_extensions?`, `doc_type?` | Import documentation (.md/.txt/.rst/.pdf) into the knowledge base. Use to index reference docs, style guides, or API manuals for `docs_search` retrieval. Supports rule extraction and auto-injection by file extension. |
| `list_docs` | `project?` | List all indexed documentation sources. Use to discover available reference docs before searching. Filter by project. |
| `remove_docs` | `name`, `project?` | Remove a documentation source and its indexed data. Use when a doc source is outdated or no longer needed. |

### Plan Management
| Tool | Parameters | Description |
|------|-----------|-------------|
| `set_plan` | `plan`, `scope?` | Set the active plan for the current thread — survives context collapse and proxy restarts. Use for any task spanning more than 5 tool cycles, exploring multiple hypotheses, or touching multiple files. Primary anchor against context loss. Pair with `update_plan`. |
| `update_plan` | `plan?`, `completed?`, `add?`, `remove?` | Update the active plan — mark items completed, add new items, or remove items. Use after each milestone or pivot to keep the plan current. The plan is your primary anchor against context loss. |
| `get_plan` | — | Get the current active plan. Use for self-orientation after context collapse or when resuming work — the plan is your context-loss-proof anchor. |
| `complete_plan` | — | Mark the active plan as completed. Use when all plan items are done — stops plan checkpoints and signals completion to the orchestration layer. |

### Agent Orchestration
| Tool | Parameters | Description |
|------|-----------|-------------|
| `spawn_agent` | `project`, `section`, `caller_session?`, `token_budget?`, `max_turns?`, `model?`, `theme?`, `work_dir?`, `backend?` | Spawn an autonomous agent for a project section. Use to delegate work to an isolated agent with its own worktree, model, and token budget. Agents run as visible TUI processes. Specify backend (`claude`|`codex`|`opencode`) and optional model override. `theme` overrides the TUI theme for this agent (empty = the theme of the agent type; see `agents.themes`). |
| `relay_agent` | `to`, `content`, `project?`, `caller_session?` | Inject content into a running agent's terminal. Use to send instructions, nudges, or context to an agent without stopping it. Target by agent ID or section name. |
| `stop_agent` | `to`, `project?` | Stop a running agent. Use to terminate an agent that has completed its work or is misbehaving. Target by agent ID. |
| `stop_all_agents` | `project` | Stop all running agents in a project. Use for cleanup or when resetting a project's agent state. |
| `resume_agent` | `to`, `project?` | Resume a previously stopped agent. Use to continue work from where a stopped agent left off. Target by agent ID or section name. |
| `list_agents` | `project?` | List all agents in a project with their status, PID, and section. Use to monitor running agents and discover their IDs for `relay_agent` or `stop_agent`. |
| `get_agent` | `to`, `project?` | Get detailed information about a specific agent. Use to check an agent's status, session ID, or worktree before sending commands via `relay_agent` or `stop_agent`. |
| `update_agent_status` | `phase`, `id?` | Update an agent's semantic work phase description. Use to signal progress through the 6-phase pipeline or to mark the current milestone. Free-form string, displayed in agent listings. |

### Scratchpad
| Tool | Parameters | Description |
|------|-----------|-------------|
| `scratchpad_write` | `project`, `section`, `content` | Write a section to the shared persistent scratchpad (upsert). Use for cross-session state, agent coordination, or storing intermediate results. Project and section are required. |
| `scratchpad_read` | `project`, `section?` | Read scratchpad sections. Use to retrieve shared state, agent briefings, or stored results. Omit section to read all sections in a project. |
| `scratchpad_list` | `project?` | List scratchpad projects and their sections. Use to discover available scratchpad state across projects. |
| `scratchpad_delete` | `project`, `section?` | Delete a scratchpad section or an entire project's scratchpad. Use for cleanup after work is done. Omit section to delete the entire project. |

### Code Intelligence
| Tool | Parameters | Description |
|------|-----------|-------------|
| `search_code_index` | `pattern`, `project`, `kind?`, `file_pattern?`, `limit?` | Search the code graph for symbols (functions, types, methods, packages) by name pattern. Use for fast symbol lookups without filesystem access. For full-text code search use `search_code`. |
| `search_code` | `pattern`, `project`, `file_pattern?`, `limit?` | Full-text search across source files, enriched with graph context. Use to find code by content — returns containing function and callers alongside matches. For symbol-by-name search use `search_code_index`. |
| `get_code_context` | `qualified_name`, `project`, `include_neighbors?` | Get detailed symbol information: signature, file location, and connected graph nodes. Use after `search_code_index` to inspect a symbol before reading its full source with `get_code_snippet`. |
| `get_code_snippet` | `project`, `qualified_name?`, `file?`, `start_line?`, `end_line?` | Retrieve full source code for a symbol or arbitrary file range. Two modes: (1) qualified_name from `search_code_index`, (2) file + start_line + end_line for arbitrary ranges. Use as your primary code-reading tool instead of shell cat/read. |
| `get_file_symbols` | `file`, `project` | List all top-level symbols in a file with line numbers. Use for quick file overview before diving into specific symbols. Returns func, method, var, const, type. |
| `get_dependency_map` | `package`, `project`, `depth?` | Generate a package import dependency graph with cycle detection. Use to understand module structure and identify circular dependencies before refactoring. |
| `graph_traverse` | `from`, `project`, `direction?`, `edge_type?`, `depth?` | Trace call paths and dependencies from a code graph node. Use to understand call chains, imports, or type definitions — follow inbound/outbound edges by type (`calls`, `imports`, `defines`). |
| `get_file_index` | `project`, `dir?` | List source files in a directory with learning and gotcha annotations. Use to browse a project's file structure and discover files with known issues or important context. |

### Capabilities
| Tool | Parameters | Description |
|------|-----------|-------------|
| `get_caps` | `project?`, `name?`, `tag?`, `limit?` | Load saved capability definitions. Use to discover available caps by name, tag, or project. Capabilities are reusable, tested tool definitions that persist across sessions. |
| `save_cap` | `name`, `scripts`, `description?`, `tags?`, `project?`, `tested?`, `test_date?`, `auto_active?`, `actions?` | Save an executable capability (tool definition) that persists across sessions. Use to create reusable tools from working REPL snippets, bash commands, or multi-step workflows. Auto-supersedes existing caps with the same name. Scripts array supports tool (REPL) and handler (bash/JS) kinds. |
| `cap_proposal_decide` | `id`, `decision`, `notes?` | Accept or reject an auto-corrected cap proposal. Use after reviewing a proposed cap correction — accept to apply the changes, reject to keep the active cap unchanged. The proposal must be in category='cap_proposed'. On accept, the proposed bash body is applied to the active cap via `save_cap` with source='auto_correct_accepted'. |
| `list_cap_proposals` | `status?`, `project?`, `limit?` | List cap-correction proposals. Use to find pending auto-correct suggestions before reviewing and deciding with `cap_proposal_decide`. Defaults to `cap_proposed` (pending review). Pass `cap_proposed_accepted`, `cap_proposed_rejected`, or `all` to see history. |
| `register_caps` | `project?`, `tag?` | Generate JavaScript registerTool() code for saved caps. Use to make caps available as native REPL tools. Execute the returned code in the REPL. Filter by project or tag. |
| `activate_cap` | `name`, `project?` | Activate a saved cap for the current thread (thread_id is auto-injected). Use to enable a capability for this session — returns registerTool() JS to paste into the REPL. Capability re-injection on subsequent turns is automatic. NOTE: call this as a native MCP tool — do NOT invoke it from the REPL. |
| `deactivate_cap` | `name` | Deactivate a cap for the current thread (thread_id is auto-injected). Use to stop capability re-injection on subsequent turns. The proxy stops re-injecting its registerTool snippet. |
| `execute_cap` | `name`, `fn?`, `args?` | Execute a saved capability handler sandboxed. Returns JSON result. Use to invoke cap tools from any context. |
| `cap_store` | `capability`, `action`, `table?`, `columns?`, `data?`, `where?`, `args?`, `limit?` | Capability database — namespaced tables for structured data. Use for persistent storage within caps (`create_table`, `upsert`, `query`, `delete`, `list_tables`, `claim_and_read`). Pass columns as JSON array [{name,type}] for create_table. Pass data as JSON object for upsert (include id to update). Pass args as JSON array of bind values for WHERE clauses. |

### Scheduling
| Tool | Parameters | Description |
|------|-----------|-------------|
| `schedule` | `action`, `name?`, `cron?`, `prompt?`, `enabled?`, `recurring?`, `id?`, `mode?`, `cap_name?`, `script_name?`, `auto_correct?`, `allowed_ports?`, `sandbox?`, `interval_seconds?`, `model?` | Create, list, delete, or run scheduled jobs. Use for recurring tasks like Telegram polling, periodic checks, or one-shot delayed work. Jobs persist across daemon restarts. Supports cron, interval, agent/headless/bash modes, and sandbox profiles. |

### Navigation Dismissal
| Tool | Parameters | Description |
|------|-----------|-------------|
| `dismiss_code_nav` | `session_id` | Dismiss a code-navigation suggestion for this session. Use when the code-nav hook blocks a shell tool but you need shell access. After 5 dismissals per session, suggestions stop until next session. |
| `dismiss_repl_pattern` | `project`, `shape_hash` | Dismiss a recorded REPL command pattern from future cap-suggestion analysis. Use when a suggested pattern is a false positive. Resets the pattern's count to 0. At 3 dismissals the pattern is flagged permanently ignored. |

---

### Auto-Injected Parameters

Every MCP tool call receives these parameters injected by `proxyCall`/`proxyCallFormat`/`proxyCallWithThreadID`:

| Parameter | Source | Description |
|-----------|--------|-------------|
| `_caller_pid` | `os.Getppid()` | Resolves session ID from daemon pidMap |
| `_session_id` | `CLAUDE_SESSION_ID` / `CODEX_THREAD_ID` / OpenCode env | Multi-agent session identity |
| `_source_agent` | env auto-detection | `claude`, `codex`, or `opencode` |
| `_client_model` | `ANTHROPIC_MODEL` / `MODEL` etc. | Current model name (in `proxyCall`/`proxyCallFormat`) |
| `_cwd` | parent process CWD | Current working directory (in `proxyCall`/`proxyCallFormat`) |

Sessions from Claude Code are unchanged (raw `CLAUDE_SESSION_ID`). Codex sessions get `codex:` prefix. OpenCode sessions get `opencode:` prefix.
