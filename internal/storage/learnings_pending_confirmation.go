package storage

import (
	"fmt"

	"github.com/carsteneu/yesmem/internal/models"
)

const (
	// PendingConfirmationListLimit caps a review listing when no limit is given.
	PendingConfirmationListLimit = 200
	// pendingConfirmationScanCap bounds the scan behind a filtered listing. The
	// retired status is a closed historical set; the cap only guards against
	// surprising growth.
	pendingConfirmationScanCap = 5000
)

// GetPendingConfirmationLearnings lists active learnings parked in the retired
// "pending_confirmation" status. Decision D6 removed the status (nothing ever
// read it); the historical rows stay untouched and this query exists only so
// they can be sifted by hand.
//
// It returns at most limit learnings plus the total number of matches, so a
// caller can tell "everything listed" from "truncated". The project filter is
// applied in Go via projectMatchesTolerant: parked rows predate the
// canonical_project backfill, so an exact canonical comparison would hide
// exactly the historical rows this query exists for.
func (s *Store) GetPendingConfirmationLearnings(project string, limit int) ([]models.Learning, int, error) {
	if limit <= 0 {
		limit = PendingConfirmationListLimit
	}
	all, err := s.scanPendingConfirmationRows()
	if err != nil {
		return nil, 0, err
	}
	if project != "" {
		matched := make([]models.Learning, 0, len(all))
		for _, l := range all {
			if projectMatchesTolerant(l.Project, l.CanonicalProject, project) {
				matched = append(matched, l)
			}
		}
		all = matched
	}
	total := len(all)
	if len(all) > limit {
		all = all[:limit]
	}
	return all, total, nil
}

func (s *Store) scanPendingConfirmationRows() ([]models.Learning, error) {
	rows, err := s.readerDB().Query(`SELECT id, session_id, category, content, project, confidence,
		superseded_by, supersede_reason, created_at, expires_at, model_used, source,
		COALESCE(hit_count, 0), COALESCE(emotional_intensity, 0.0), last_hit_at, COALESCE(session_flavor, ''), valid_until, supersedes, COALESCE(importance, 3), supersede_status, COALESCE(noise_count, 0), COALESCE(fail_count, 0),
		COALESCE(match_count, 0), COALESCE(inject_count, 0), COALESCE(use_count, 0), COALESCE(save_count, 0), COALESCE(stability, 30.0),
		COALESCE(context, ''), COALESCE(domain, 'code'), COALESCE(trigger_rule, ''), COALESCE(embedding_text, ''),
		COALESCE(source_file, ''), COALESCE(source_hash, ''), COALESCE(doc_chunk_ref, 0), COALESCE(task_type, ''), COALESCE(turns_at_creation, 0), COALESCE(origin_tool, ''), COALESCE(source_msg_from, -1), COALESCE(source_msg_to, -1),
		COALESCE(canonical_project, ''),
		COALESCE(attribution, ''),
		COALESCE(staleness_score, 0.0), COALESCE(staleness_reason, ''), staleness_checked_at, COALESCE(staleness_type, ''), COALESCE(code_fingerprint, ''), COALESCE(project_source, '')
		FROM learnings
		WHERE supersede_status = 'pending_confirmation' AND superseded_by IS NULL
		ORDER BY created_at DESC LIMIT ?`, pendingConfirmationScanCap)
	if err != nil {
		return nil, fmt.Errorf("get pending confirmation learnings: %w", err)
	}
	defer rows.Close()
	return scanLearnings(rows)
}
