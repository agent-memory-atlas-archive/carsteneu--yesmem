package extraction

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/carsteneu/yesmem/internal/models"
)

// The volatile sentinel guards the learnings table against volatile snapshots:
// archived live-derivable or rotting state values instead of durable knowledge.
// The loose recruit heuristic (LooksLikeStateSnapshot) decides WHEN to ask;
// the LLM sentinel provides the actual judgment.

const VolatileSentinelSystemPrompt = `You classify knowledge items as volatile snapshots or durable knowledge.

A learning is VOLATILE when its core claim is a live-derivable or rotting state value:
- bestands-/health counts ("70 MCP tools", "4961 pings", "6 restarts", "161 files", cache-hit ratio %)
- runtime state ("proxy is in degraded state", "X was connected")
- config values ("reply_model is deepseek-v4-pro")
- version/binary snapshots (SHA-256, binary size, "N commits ahead")

A learning is DURABLE when it carries an insight, rule, preference, workaround or decision
that survives state changes — even if it mentions numbers in passing.
Dated research facts ("X had 77.7k stars as of May 2026") are DURABLE reference points
when the date anchor is explicit.

Verdicts:
- "volatile" — pure state snapshot, derivable live or rotting silently
- "bounded"  — snapshot value plus real insight; useful for a limited time
- "durable"  — knowledge that survives state changes

Respond ONLY with a JSON array:
[{"id": <int>, "verdict": "volatile|bounded|durable", "confidence": <0.0-1.0>, "reason": "<brief>"}]`

// VolatileVerdict is the sentinel's judgment on a single learning.
type VolatileVerdict struct {
	ID         int64   `json:"id"`
	Verdict    string  `json:"verdict"` // volatile | bounded | durable
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

// LooksLikeStateSnapshot is the cheap recruiter: true when the content plausibly
// carries a state/count value. It only gates WHEN the LLM sentinel runs, so a
// loose recall is safe — the sentinel provides the actual judgment.
func LooksLikeStateSnapshot(content string) bool {
	if !stateSnapRe.MatchString(content) {
		return false
	}
	s := strings.ToLower(content)
	for _, m := range []string{
		"currently", "right now", "as of", "aktuell", "derzeit",
		"restarts", "degraded", "uptime", "pings",
		" tools", "sessions", "commits ahead", "hit ratio", "hit-ratio",
		"stars", "binary size", "sha-256", "version ",
		"model is", "model was",
	} {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// stateSnapRe requires a number attached to a count-unit (tools, sessions,
// files, …) or an explicit state word — a bare digit alone suffices for the
// current-state phrasings ("currently", "as of") that follow.
var stateSnapRe = regexp.MustCompile(`(?i)(\b\d+ ?\+? ?(mcp[- ]? )?(tools?|sessions?|files?|restarts?|pings?|commits? ahead|stars)\b)|(\b(degraded|uptime)\b)|((currently|right now|as of|aktuell|derzeit) )`)

// ClassifyVolatileBatch asks the LLM to classify a batch of learnings.
func ClassifyVolatileBatch(client LLMClient, batch []models.Learning) ([]VolatileVerdict, error) {
	if len(batch) == 0 {
		return nil, nil
	}
	var lines []string
	for _, l := range batch {
		content := l.Content
		if len(content) > 300 {
			content = content[:300] + "..."
		}
		lines = append(lines, fmt.Sprintf("[ID:%d] %s", l.ID, content))
	}
	userMsg := fmt.Sprintf("## Learnings\n%s\n\nFor each learning: volatile, bounded, or durable?",
		strings.Join(lines, "\n"))

	response, err := client.Complete(VolatileSentinelSystemPrompt, userMsg, WithMaxTokens(12000))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(response) == "" {
		return nil, fmt.Errorf("empty response from %s", client.Model())
	}

	response = strings.TrimSpace(response)
	response = strings.TrimPrefix(response, "```json")
	response = strings.TrimPrefix(response, "```")
	response = strings.TrimSuffix(response, "```")
	response = strings.TrimSpace(response)

	var verdicts []VolatileVerdict
	if err := json.Unmarshal([]byte(response), &verdicts); err != nil {
		return nil, fmt.Errorf("parse sentinel response: %w", err)
	}
	return verdicts, nil
}
