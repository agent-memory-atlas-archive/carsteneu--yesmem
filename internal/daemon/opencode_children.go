package daemon

import (
	"context"
	"fmt"
	"time"
)

// --- opencode child-session awareness for heartbeat checks ---
//
// task() subagents of an opencode-backed loop agent appear in opencode's
// session store as rows with parent_id set to the parent session id (v1:
// table `session`, v2: table `session_v2`). While such a child is active, the
// agent cannot consume injected relays (it is blocked inside the subagent
// call), so heartbeat-driven layers must not relay: injected messages would
// queue as pending user messages and flood the agent after the subagent
// returns. Access pattern mirrors handler_agents.go (sqlite3 CLI, no CGO
// driver); queryFirstSessionRow consults both tables.

// opencodeActiveChildWindow is how long after a child session's last update we
// still consider it active. Best-effort heuristic: a child that runs longer
// than this window without touching its row reads as inactive, in which case
// one relay queues as a pending message — belated, not lost.
const opencodeActiveChildWindow = 90 * time.Second

// opencodeChildQueryTimeout bounds the whole child lookup so a stuck query
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
// freshness window. Both session tables are consulted, so v2 subagents count.
// Any I/O or schema error reads as "no active child" — this gate only
// suppresses relaying, it must never break the heartbeat.
func opencodeChildSessionActive(dbPath, parentSessionID string) bool {
	if dbPath == "" || parentSessionID == "" {
		return false
	}
	cutoff := time.Now().Add(-opencodeActiveChildWindow).UnixMilli()
	// One budget for the whole lookup: this runs in the shared heartbeat
	// goroutine (relay gating), so the number of session tables must not
	// multiply the wait.
	ctx, cancel := context.WithTimeout(context.Background(), opencodeChildQueryTimeout)
	defer cancel()
	// The parent id is the only caller-supplied value here; it is bound, so a
	// crafted id cannot become SQL. cutoff is derived from the clock.
	row, err := queryFirstSessionRowParams(ctx, dbPath, func(table string) string {
		return fmt.Sprintf("SELECT 1 FROM %s WHERE parent_id = @parent AND time_updated > %d LIMIT 1",
			table, cutoff)
	}, []sessionQueryParam{{name: "@parent", value: parentSessionID}})
	if err != nil {
		return false
	}
	return row != ""
}
