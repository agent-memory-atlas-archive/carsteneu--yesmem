package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"
)

// evidenceLookupTimeout bounds one session lookup for the DONE-guard's Phase 5
// evidence check. The check runs inside the daemon's heartbeat goroutine once
// per evidence key, so the whole lookup is bounded here rather than per table.
const evidenceLookupTimeout = 5 * time.Second

// allowedReviewAgents maps each Phase 5 evidence key to the opencode agent
// names permitted to fill that slot. The task() dispatches behind the ids are
// agent-typed in opencode's session table; the sets below pin which agent type
// may serve which review sub-phase.
var allowedReviewAgents = map[string][]string{
	"cold-review": {"reviewer", "silent-bob"},
	"consequence": {"general", "reviewer"},
	"security":    {"reviewer"},
}

// phase5EvidenceOrder pins the evidence key order so validation errors are
// deterministic despite map iteration.
var phase5EvidenceOrder = []string{"cold-review", "consequence", "security"}

// queryOpencodeSession looks up a session row in the opencode session database.
// found=false means the row does not exist (agent fault). An error means the
// lookup itself failed — locked DB, missing sqlite3 binary, schema drift — an
// infrastructure problem, not an agent fault. Variable so tests can stub the
// lookup hermetically.
//
// Both session tables are consulted (v1 `session`, v2 `session_v2`): v2 agents
// have a captured session id in `session_v2` only, so without the fallback the
// evidence check would reject every v2 agent's Phase 5 block (Learning #101594).
var queryOpencodeSession = func(ocDBPath, sessionID string) (found bool, parentID, agentType string, err error) {
	escaped := strings.ReplaceAll(sessionID, "'", "''")
	// A total budget for the whole lookup: this runs inside the shared heartbeat
	// goroutine once per evidence key, so it must not multiply by the number of
	// session tables.
	ctx, cancel := context.WithTimeout(context.Background(), evidenceLookupTimeout)
	defer cancel()
	row, err := queryFirstSessionRow(ctx, ocDBPath, func(table string) string {
		return fmt.Sprintf("SELECT parent_id, agent FROM %s WHERE id = '%s' LIMIT 1", table, escaped)
	})
	if err != nil {
		return false, "", "", err
	}
	if row == "" {
		return false, "", "", nil
	}
	parts := strings.SplitN(row, "|", 2)
	if len(parts) != 2 {
		return false, "", "", fmt.Errorf("unexpected sqlite3 output %q", row)
	}
	return true, parts[0], parts[1], nil
}

// VerifySubagentEvidence checks Phase 5 evidence session ids against the
// opencode session database. evidence maps the sub-phase key (cold-review,
// consequence, security) to the reported session id. Per key: row exists,
// row's parent_id matches parentSessionID, agent type is in the allowed set;
// plus pairwise-distinct ids across keys. An empty ocDBPath or parentSessionID
// skips verification — that is a configuration, not a failure: the check cannot
// run at all. A named database that cannot be read, and a lookup that fails, are
// findings: an unverifiable id is not a verified one, and the report is the only
// thing the DONE claim rests on (YM-304). The guard pauses the agent on a finding
// and unpauses it once the scratchpad is compliant, so the closed direction is the
// one that heals itself.
func VerifySubagentEvidence(ocDBPath, parentSessionID string, evidence map[string]string) []FieldError {
	if ocDBPath == "" || parentSessionID == "" || len(evidence) == 0 {
		return nil
	}
	if _, err := os.Stat(ocDBPath); err != nil {
		log.Printf("[done-guard] evidence check cannot run: opencode.db unreadable at %s: %v", ocDBPath, err)
		return []FieldError{{
			Phase: 5,
			Field: "cold-review/consequence/security",
			Detail: fmt.Sprintf("INFRASTRUCTURE, not your report: the Phase 5 evidence could not be verified because opencode.db is unreadable at %s (%v). "+
				"Fixing the block is not possible from here; the guard clears once the lookup works again.", ocDBPath, err),
		}}
	}

	var errs []FieldError
	// Every value below is agent-influenced: sid comes from the agent's own
	// scratchpad, parentID and agentType from opencode's session table. The
	// Detail reaches relay bodies and orchestrator messages, so it is neutralized
	// here, at the point where the value enters the message (YM-285).
	for i, key := range phase5EvidenceOrder {
		sid, ok := evidence[key]
		if !ok || sid == "" {
			continue
		}
		for _, prev := range phase5EvidenceOrder[:i] {
			if evidence[prev] != "" && evidence[prev] == sid {
				errs = append(errs, FieldError{
					Phase: 5,
					Field: key,
					Detail: fmt.Sprintf("session id %s duplicated across %s and %s — each sub-phase needs its own task() dispatch",
						safeAgentText(sid), key, prev),
				})
				break
			}
		}
		found, parentID, agentType, err := queryOpencodeSession(ocDBPath, sid)
		if err != nil {
			log.Printf("[done-guard] evidence check failed for %s: lookup of %s failed: %v", key, safeAgentText(sid), err)
			errs = append(errs, FieldError{
				Phase: 5,
				Field: key,
				Detail: fmt.Sprintf("INFRASTRUCTURE, not your report: subagent session %s could not be verified because the lookup failed (%v). "+
					"Rewriting the phase block does not help; the guard clears once the lookup works again.", safeAgentText(sid), err),
			})
			continue
		}
		if !found {
			errs = append(errs, FieldError{
				Phase:  5,
				Field:  key,
				Detail: fmt.Sprintf("subagent session %s not found in opencode.db", safeAgentText(sid)),
			})
			continue
		}
		if parentID != parentSessionID {
			errs = append(errs, FieldError{
				Phase: 5,
				Field: key,
				Detail: fmt.Sprintf("subagent session %s has parent_id %q, expected the agent's own session %q",
					safeAgentText(sid), safeAgentText(parentID), safeAgentText(parentSessionID)),
			})
		}
		if !agentAllowed(key, agentType) {
			errs = append(errs, FieldError{
				Phase:  5,
				Field:  key,
				Detail: fmt.Sprintf("subagent session %s has agent %q, not in allowed set %v", safeAgentText(sid), safeAgentText(agentType), allowedReviewAgents[key]),
			})
		}
	}
	return errs
}

// agentAllowed reports whether agentType may serve the given evidence key.
func agentAllowed(key, agentType string) bool {
	for _, a := range allowedReviewAgents[key] {
		if a == agentType {
			return true
		}
	}
	return false
}
