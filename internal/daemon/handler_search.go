package daemon

import (
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

func (h *Handler) handleSearch(params map[string]any) Response {
	query, _ := params["query"].(string)
	queryEn, _ := params["query_en"].(string)
	project, _ := params["project"].(string)
	excludeSession, _ := params["exclude_session"].(string)
	since, _ := params["since"].(string)
	before, _ := params["before"].(string)
	limit := intOr(params, "limit", 10)

	// Bilingual search: when query_en is set, run both queries with RRF fusion
	if queryEn != "" {
		hits, err := h.store.SearchMessagesBilingualCtx(query, queryEn, since, before, limit*3)
		if err != nil {
			return errorResponse(err.Error())
		}
		return h.filterSearchResults(hits, project, excludeSession, limit)
	}

	hits, err := h.store.SearchMessagesCtx(query, since, before, limit*3) // over-fetch for project filter
	if err != nil {
		return errorResponse(err.Error())
	}

	return h.filterSearchResults(hits, project, excludeSession, limit)
}

// filterSearchResults is the common post-processing for search results:
// project filtering, session exclusion, snippet truncation, and enrichment.
func (h *Handler) filterSearchResults(hits []storage.MessageSearchResult, project, excludeSession string, limit int) Response {
	sessionIDs := make([]string, 0, len(hits))
	seen := make(map[string]bool)
	for _, hit := range hits {
		if !seen[hit.SessionID] {
			seen[hit.SessionID] = true
			sessionIDs = append(sessionIDs, hit.SessionID)
		}
	}
	sessionMeta, _ := h.store.GetSessionMetaBulk(sessionIDs)

	var results []models.SearchResult
	for _, hit := range hits {
		if len(results) >= limit {
			break
		}
		if excludeSession != "" && hit.SessionID == excludeSession {
			continue
		}
		proj := ""
		if m, ok := sessionMeta[hit.SessionID]; ok {
			proj = m.Project
		}
		if project != "" && !models.ProjectMatches(proj, project) {
			continue
		}
		snippet := hit.Content
		if len(snippet) > 300 {
			snippet = snippet[:300] + "..."
		}
		results = append(results, models.SearchResult{
			SessionID:   hit.SessionID,
			Project:     proj,
			Timestamp:   hit.Timestamp,
			Snippet:     snippet,
			Score:       -hit.Rank,
			MessageType: hit.MessageType,
			SourceAgent: hit.SourceAgent,
		})
	}
	h.annotateSubagentInfo(results)
	return jsonResponse(results)
}

// annotateSubagentInfo enriches search results with subagent metadata from the DB.
func (h *Handler) annotateSubagentInfo(results []models.SearchResult) {
	if len(results) == 0 {
		return
	}
	seen := make(map[string]bool, len(results))
	var ids []string
	for _, r := range results {
		if r.SessionID != "" && !seen[r.SessionID] {
			seen[r.SessionID] = true
			ids = append(ids, r.SessionID)
		}
	}
	meta, err := h.store.GetSessionMetaBulk(ids)
	if err != nil || len(meta) == 0 {
		return
	}
	for i := range results {
		if m, ok := meta[results[i].SessionID]; ok {
			results[i].AgentType = m.AgentType
			results[i].ParentSessionID = m.ParentSessionID
			results[i].SourceAgent = m.SourceAgent
		}
	}
}

func (h *Handler) handleDeepSearch(params map[string]any) Response {
	query, _ := params["query"].(string)
	queryEn, _ := params["query_en"].(string)
	project, _ := params["project"].(string)
	excludeSession, _ := params["exclude_session"].(string)
	since, _ := params["since"].(string)
	before, _ := params["before"].(string)
	limit := intOr(params, "limit", 5)
	includeThinking, _ := params["include_thinking"].(bool)
	includeCommands, _ := params["include_commands"].(bool)

	t0 := time.Now()
	ftsStart := time.Now()

	var hits []storage.MessageSearchResult
	var err error
	if queryEn != "" {
		// Bilingual deep search: storage-side RRF fusion (dedup by ID,
		// deterministic tiebreak). Pass limit*3 so the downstream project
		// filter sees the same pool size as the standard path — truncating
		// here would starve the filter when multiple hits share a project.
		hits, err = h.store.SearchMessagesDeepBilingualCtx(query, queryEn, includeThinking, includeCommands, since, before, limit*3)
		if err != nil {
			return errorResponse(err.Error())
		}
	} else {
		// Standard path — FTS5 now indexes thinking blocks (content_blob copied to content),
		// so we search all types directly — no separate enrichment queries needed
		hits, err = h.store.SearchMessagesDeepCtx(query, includeThinking, includeCommands, since, before, limit*3)
		if err != nil {
			return errorResponse(err.Error())
		}
	}
	ftsMs := time.Since(ftsStart).Milliseconds()

	ctxStart := time.Now()

	// Batch-load session metadata (single query instead of N+1)
	sessionIDs := make([]string, 0, len(hits))
	seen := make(map[string]bool)
	for _, hit := range hits {
		if !seen[hit.SessionID] {
			seen[hit.SessionID] = true
			sessionIDs = append(sessionIDs, hit.SessionID)
		}
	}
	sessionMeta, _ := h.store.GetSessionMetaBulk(sessionIDs)

	type enriched struct {
		SessionID       string  `json:"session_id"`
		Project         string  `json:"project"`
		Timestamp       string  `json:"timestamp"`
		Snippet         string  `json:"snippet"`
		Context         string  `json:"context,omitempty"`
		Score           float64 `json:"score"`
		MessageType     string  `json:"message_type"`
		AgentType       string  `json:"agent_type,omitempty"`
		ParentSessionID string  `json:"parent_session_id,omitempty"`
		SourceAgent     string  `json:"source_agent,omitempty"`
	}

	msgDB := h.store.MessagesDB()

	// Batch-load ±3 context windows: one query per session (not per hit)
	type ctxMsg struct{ seq int; content, ts string }
	ctxBySession := make(map[string][]ctxMsg)
	if msgDB != nil {
		type sessionRange struct{ min, max int }
		ranges := make(map[string]*sessionRange)
		for _, hit := range hits {
			r := ranges[hit.SessionID]
			if r == nil {
				r = &sessionRange{min: hit.Sequence - 3, max: hit.Sequence + 3}
				ranges[hit.SessionID] = r
			} else {
				if n := hit.Sequence - 3; n < r.min {
					r.min = n
				}
				if n := hit.Sequence + 3; n > r.max {
					r.max = n
				}
			}
		}
		for sid, r := range ranges {
			// The body of a thinking or tool_result message can sit only in
			// content_blob with content empty (migrated rows), so the window falls
			// back to the blob or the neighbour renders as a blank line.
			rows, err := msgDB.Query(
				`SELECT sequence, COALESCE(NULLIF(content, ''), CAST(content_blob AS TEXT), ''), timestamp
				 FROM messages WHERE session_id = ? AND sequence BETWEEN ? AND ? ORDER BY sequence`,
				sid, r.min, r.max)
			if err != nil {
				continue
			}
			for rows.Next() {
				var m ctxMsg
				rows.Scan(&m.seq, &m.content, &m.ts)
				ctxBySession[sid] = append(ctxBySession[sid], m)
			}
			rows.Close()
		}
	}

	var out []enriched
	const maxCtxSize = 10000
	for _, hit := range hits {
		if len(out) >= limit {
			break
		}
		if excludeSession != "" && hit.SessionID == excludeSession {
			continue
		}
		proj := ""
		agentType := ""
		parentSID := ""
		sourceAgent := hit.SourceAgent
		if m, ok := sessionMeta[hit.SessionID]; ok {
			proj = m.Project
			agentType = m.AgentType
			parentSID = m.ParentSessionID
			if sourceAgent == "" {
				sourceAgent = m.SourceAgent
			}
		}
		if project != "" && !models.ProjectMatches(proj, project) {
			continue
		}
		snippet := ""
		contextWindow := ""
		if msgs, ok := ctxBySession[hit.SessionID]; ok {
			var sb strings.Builder
			for _, m := range msgs {
				if m.seq < hit.Sequence-3 || m.seq > hit.Sequence+3 {
					continue
				}
				// The blob fallback above can surface a whole thinking block, so both
				// the snippet and the context line are bounded; otherwise one hit
				// carries a multi-megabyte body into the response.
				content := m.content
				if len(content) > 4000 {
					content = content[:4000] + "..."
				}
				if m.seq == hit.Sequence {
					snippet = content
				} else if len(content) > 1000 {
					content = content[:1000] + "..."
				}
				date := ""
				if len(m.ts) >= 10 {
					date = "[" + m.ts[:10] + "] "
				}
				if m.seq == hit.Sequence {
					fmt.Fprintf(&sb, "%s>>> %s\n", date, content)
				} else {
					fmt.Fprintf(&sb, "%s%s\n", date, content)
				}
				if sb.Len() > maxCtxSize {
					break
				}
			}
			contextWindow = sb.String()
		}
		out = append(out, enriched{
			SessionID: hit.SessionID, Project: proj, Timestamp: hit.Timestamp,
			Snippet: snippet, Context: contextWindow, Score: -hit.Rank, MessageType: hit.MessageType,
			AgentType: agentType, ParentSessionID: parentSID, SourceAgent: sourceAgent,
		})
	}
	ctxMs := time.Since(ctxStart).Milliseconds()
	totalMs := time.Since(t0).Milliseconds()
	log.Printf("deep_search: q='%s' fts=%dms ctx=%dms n=%d total=%dms", query, ftsMs, ctxMs, len(out), totalMs)
	return jsonResponse(out)
}
