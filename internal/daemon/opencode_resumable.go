package daemon

import (
	"context"
	"fmt"
)

// opencodeSessionResumableFn reports whether a backend session still exists in
// opencode's store for the agent's own work dir, i.e. whether resume_agent can
// reattach to it. Injectable so tests stay hermetic; the suite pins it to "not
// resumable" in TestMain.
var opencodeSessionResumableFn = func(sessionID, workDir string) (bool, error) {
	return opencodeSessionResumable(opencodeDBPath(), sessionID, workDir)
}

// opencodeSessionResumable answers whether opencode knows the given session for
// the given directory.
//
// This is the daemon-restart recovery's question, not a "is the agent working"
// test: an opencode session outlives the window and the PTY bridge that started
// it, and it keeps answering turns inside the shared opencode service (Learning
// #101526). A row pointing at such a session must not be deleted on restart
// (YM-297).
//
// Existence alone is not identity: agents.opencode_session_id can be stale and
// shared across rows (Learning #101745), so the session's own directory has to
// match the agent's work dir. A session id that resolves to somebody else's
// checkout is not this row's session, however well it fits.
//
// An error means "could not determine", which the caller must treat as "keep the
// row": deletion is the harm this lookup exists to avoid, and an unreadable
// store is no evidence that a session is gone. Both session tables are consulted,
// so v2 sessions count; the id may carry the "opencode:" prefix the MCP layer
// attaches, while the tables store bare ids.
func opencodeSessionResumable(dbPath, sessionID, workDir string) (bool, error) {
	sessionID = stripSessionPrefix(sessionID)
	if dbPath == "" {
		return false, fmt.Errorf("no opencode database path")
	}
	if sessionID == "" {
		return false, fmt.Errorf("empty session id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionTableQueryTimeout)
	defer cancel()
	query := func(table string) string {
		sql := fmt.Sprintf("SELECT id FROM %s WHERE id = @sid", table)
		if workDir != "" {
			sql += " AND directory = @dir"
		}
		return sql
	}
	params := []sessionQueryParam{{name: "@sid", value: sessionID}}
	if workDir != "" {
		params = append(params, sessionQueryParam{name: "@dir", value: workDir})
	}
	row, err := queryFirstSessionRowParams(ctx, dbPath, query, params)
	if err != nil {
		return false, err
	}
	return row != "", nil
}
