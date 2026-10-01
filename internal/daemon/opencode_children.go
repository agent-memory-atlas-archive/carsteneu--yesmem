package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// --- opencode child-session awareness for heartbeat checks ---
//
// task() subagents of an opencode-backed loop agent appear in the opencode
// session database (opencode.db, table `session`) as rows with parent_id set
// to the parent session id. While such a child is active, the agent cannot
// consume injected relays (it is blocked inside the subagent call), so
// heartbeat-driven layers must not relay: injected messages would queue as
// pending user messages and flood the agent after the subagent returns.
// Access pattern mirrors handler_agents.go (sqlite3 CLI, no CGO driver).

// opencodeActiveChildWindow is how long after a child session's last update we
// still consider it active. Best-effort heuristic: a child that runs longer
// than this window without touching its row reads as inactive, in which case
// one relay queues as a pending message — belated, not lost.
const opencodeActiveChildWindow = 90 * time.Second

// opencodeChildQueryTimeout bounds the sqlite3 subprocess so a stuck query
// cannot stall the shared heartbeat loop.
const opencodeChildQueryTimeout = 3 * time.Second

// opencodeChildActiveFn reports whether the given opencode parent session id
// currently has an active child session. Injectable so tests stay hermetic.
var opencodeChildActiveFn = func(parentSessionID string) bool {
	dbPath := opencodeDBPath()
	if dbPath == "" || parentSessionID == "" {
		return false
	}
	return opencodeChildSessionActive(dbPath, parentSessionID)
}

// opencodeChildSessionActive queries opencode.db directly: a child is active
// when a session row with parent_id = parentSessionID was updated within the
// freshness window. Any I/O or schema error reads as "no active child" — this
// gate only suppresses relaying, it must never break the heartbeat.
func opencodeChildSessionActive(dbPath, parentSessionID string) bool {
	if dbPath == "" || parentSessionID == "" {
		return false
	}
	if _, err := os.Stat(dbPath); err != nil {
		return false
	}
	cutoff := time.Now().Add(-opencodeActiveChildWindow).UnixMilli()
	// sqlite3 CLI does not support ? placeholders; escape the session id manually.
	escaped := strings.ReplaceAll(parentSessionID, "'", "''")
	query := fmt.Sprintf(
		"SELECT 1 FROM session WHERE parent_id = '%s' AND time_updated > %d LIMIT 1",
		escaped, cutoff)
	ctx, cancel := context.WithTimeout(context.Background(), opencodeChildQueryTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sqlite3", dbPath, query).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}
