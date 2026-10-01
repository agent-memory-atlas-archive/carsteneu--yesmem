package daemon

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

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
var queryOpencodeSession = func(ocDBPath, sessionID string) (found bool, parentID, agentType string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	escaped := strings.ReplaceAll(sessionID, "'", "''")
	query := fmt.Sprintf("SELECT parent_id, agent FROM session WHERE id = '%s' LIMIT 1", escaped)
	out, err := exec.CommandContext(ctx, "sqlite3", ocDBPath, query).Output()
	if err != nil {
		return false, "", "", err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return false, "", "", nil
	}
	parts := strings.SplitN(line, "|", 2)
	if len(parts) != 2 {
		return false, "", "", fmt.Errorf("unexpected sqlite3 output %q", line)
	}
	return true, parts[0], parts[1], nil
}

// VerifySubagentEvidence checks Phase 5 evidence session ids against the
// opencode session database. evidence maps the sub-phase key (cold-review,
// consequence, security) to the reported session id. Per key: row exists,
// row's parent_id matches parentSessionID, agent type is in the allowed set;
// plus pairwise-distinct ids across keys. An empty ocDBPath or parentSessionID
// skips verification — unverifiable is not invalid. An unreadable database or
// failed lookup is an infrastructure problem: it is logged and skipped so
// agents are not paused on infra outages.
func VerifySubagentEvidence(ocDBPath, parentSessionID string, evidence map[string]string) []FieldError {
	if ocDBPath == "" || parentSessionID == "" || len(evidence) == 0 {
		return nil
	}
	if _, err := os.Stat(ocDBPath); err != nil {
		log.Printf("[done-guard] evidence check skipped: opencode.db unreadable at %s: %v", ocDBPath, err)
		return nil
	}

	var errs []FieldError
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
						sid, key, prev),
				})
				break
			}
		}
		found, parentID, agentType, err := queryOpencodeSession(ocDBPath, sid)
		if err != nil {
			// Infra failure (locked DB, missing sqlite3, timeout): unverifiable
			// is not invalid — skip this key instead of failing the agent.
			log.Printf("[done-guard] evidence check skipped for %s: lookup of %s failed: %v", key, sid, err)
			continue
		}
		if !found {
			errs = append(errs, FieldError{
				Phase:  5,
				Field:  key,
				Detail: fmt.Sprintf("subagent session %s not found in opencode.db", sid),
			})
			continue
		}
		if parentID != parentSessionID {
			errs = append(errs, FieldError{
				Phase: 5,
				Field: key,
				Detail: fmt.Sprintf("subagent session %s has parent_id %q, expected the agent's own session %q",
					sid, parentID, parentSessionID),
			})
		}
		if !agentAllowed(key, agentType) {
			errs = append(errs, FieldError{
				Phase:  5,
				Field:  key,
				Detail: fmt.Sprintf("subagent session %s has agent %q, not in allowed set %v", sid, agentType, allowedReviewAgents[key]),
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
