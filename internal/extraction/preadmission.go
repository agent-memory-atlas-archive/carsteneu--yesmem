package extraction

import (
	"log"
	"strconv"

	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/textutil"
)

// PreAdmissionAction describes what to do with a learning before insert.
type PreAdmissionAction int

const (
	PreAdmissionInsert PreAdmissionAction = iota
	PreAdmissionSkip
	PreAdmissionUpdate
)

// PreAdmissionResult holds the decision for a single learning.
type PreAdmissionResult struct {
	Action     PreAdmissionAction
	ExistingID int64
	Reason     string
}

// CheckPreAdmission checks a learning against existing active learnings.
func CheckPreAdmission(store *storage.Store, learning *models.Learning) PreAdmissionResult {
	// Raw bash transcripts are session artifacts, not durable knowledge.
	if textutil.IsRawBashTranscript(learning.Content) {
		return PreAdmissionResult{Action: PreAdmissionSkip, Reason: "raw bash transcript"}
	}

	// The conflict branches below compare agent roles, and this runs before any
	// insert, so the new learning cannot carry a role yet: without this resolution
	// both sides were always empty and the cross-agent branches were dead code
	// (YM-74).
	if learning.AgentRole == "" && learning.SessionID != "" {
		learning.AgentRole = store.SessionAgentRole(learning.SessionID)
	}

	// Phase 0: Exact content hash check
	if existing, err := store.GetLearningByContentHash(textutil.ContentHash(learning.Content)); err == nil && existing != nil {
		return PreAdmissionResult{Action: PreAdmissionSkip, ExistingID: existing.ID, Reason: "exact duplicate"}
	}

	// Phase 1: BM25 search for candidates
	candidates, err := store.SearchLearningsBM25(learning.Content, learning.Project, 20)
	if err != nil {
		log.Printf("pre-admission: BM25 search failed: %v", err)
	}

	// Phase 2: Check BM25 candidates via BigramJaccard + TokenSimilarity
	if result := checkCandidates(store, learning, candidates); result != nil {
		return *result
	}

	// Phase 3: Fallback — scan category directly (catches cases BM25 misses
	// due to stemming/conjugation differences). Limited to 100 for performance.
	catLearnings, err := store.GetActiveLearnings(learning.Category, learning.Project, "", "", 0)
	if err == nil && len(catLearnings) > 0 {
		limit := len(catLearnings)
		if limit > 100 {
			limit = 100
		}
		var fallbackCandidates []storage.LearningSearchResult
		for _, l := range catLearnings[:limit] {
			fallbackCandidates = append(fallbackCandidates, storage.LearningSearchResult{
				ID:      strconv.FormatInt(l.ID, 10),
				Content: l.Content,
			})
		}
		if result := checkCandidates(store, learning, fallbackCandidates); result != nil {
			return *result
		}
	}

	return PreAdmissionResult{Action: PreAdmissionInsert}
}

// ApplyPreAdmission runs CheckPreAdmission for each candidate and returns the
// batch to insert. Single-pass and two-pass ExtractAndStore share this path so
// both write flows dedup identically.
func ApplyPreAdmission(store *storage.Store, valid []*models.Learning, sessionID string) []*models.Learning {
	var toInsert []*models.Learning
	var preSkipped, preUpdated int
	for _, l := range valid {
		result := CheckPreAdmission(store, l)
		switch result.Action {
		case PreAdmissionSkip:
			preSkipped++
		case PreAdmissionUpdate:
			if err := store.UpdateLearningContent(result.ExistingID, l.Content); err == nil {
				store.IncrementMatchCounts([]int64{result.ExistingID})
				preUpdated++
			} else {
				toInsert = append(toInsert, l)
			}
		case PreAdmissionInsert:
			toInsert = append(toInsert, l)
		}
	}
	if preSkipped > 0 || preUpdated > 0 {
		log.Printf("Pre-admission: %d skipped, %d updated, %d new (session %s)", preSkipped, preUpdated, len(toInsert), truncID(sessionID))
	}
	return toInsert
}

// checkCandidates evaluates a list of candidates against a new learning.
// Returns conflict flag when similar content exists from a different agent_role.
func checkCandidates(store *storage.Store, learning *models.Learning, candidates []storage.LearningSearchResult) *PreAdmissionResult {
	newTokens := textutil.Tokenize(learning.Content)

	// The existing side needs the same provenance: GetLearning does not read
	// agent_role, so the row's role comes from the bulk lookup (YM-74).
	candidateIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		candidateIDs = append(candidateIDs, c.ID)
	}
	existingRoles, _ := store.GetAgentRolesBulk(candidateIDs)

	for _, candidate := range candidates {
		id, err := strconv.ParseInt(candidate.ID, 10, 64)
		if err != nil {
			continue
		}
		existing, err := store.GetLearning(id)
		if err != nil || existing == nil || existing.SupersededBy != nil {
			continue
		}
		existingRole := existingRoles[candidate.ID]
		if existing.Category != learning.Category {
			continue
		}

		jaccard := BigramJaccard(learning.Content, existing.Content)
		if jaccard > 0.85 {
			// High similarity, same topic — check if different agent wrote it
			if learning.AgentRole != "" && existingRole != "" && learning.AgentRole != existingRole {
				log.Printf("pre-admission: conflict detected — new (%s) vs existing #%d (%s), jaccard=%.2f",
					learning.AgentRole, existing.ID, existingRole, jaccard)
				// Still skip the duplicate, but log the cross-agent conflict
			}
			return &PreAdmissionResult{Action: PreAdmissionSkip, ExistingID: existing.ID, Reason: "near-duplicate (jaccard)"}
		}

		existTokens := textutil.Tokenize(existing.Content)
		sim := textutil.TokenSimilarity(newTokens, existTokens)
		if sim >= 0.5 {
			// Medium similarity — same topic, different wording
			if learning.AgentRole != "" && existingRole != "" && learning.AgentRole != existingRole {
				// Different agent, same topic, different content = potential conflict
				// Allow both to exist — don't skip, don't update
				log.Printf("pre-admission: cross-agent divergence — new (%s) vs #%d (%s), sim=%.2f — keeping both",
					learning.AgentRole, existing.ID, existingRole, sim)
				return nil // insert as new, don't deduplicate
			}
			newLen := len([]rune(learning.Content))
			existLen := len([]rune(existing.Content))
			if newLen > existLen*3/2 {
				// A user-owned row is not raw material for an enrichment: fresh
				// extraction cannot rebuild an agreement, so its wording stays
				// even when the extracted text is longer (YM-362).
				if storage.IsUserOwnedSource(existing.Source) {
					return &PreAdmissionResult{Action: PreAdmissionSkip, ExistingID: existing.ID, Reason: "user-owned row kept"}
				}
				return &PreAdmissionResult{Action: PreAdmissionUpdate, ExistingID: existing.ID, Reason: "enrichment update"}
			}
			return &PreAdmissionResult{Action: PreAdmissionSkip, ExistingID: existing.ID, Reason: "already known (token similarity)"}
		}
	}

	return nil
}
