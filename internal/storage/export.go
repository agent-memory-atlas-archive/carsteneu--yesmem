package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/carsteneu/yesmem/internal/models"
)

// exportVersion is the format version ExportLearnings writes. ImportLearnings
// accepts everything up to it, so v1 files (and files without a version field)
// still import.
const exportVersion = 2

// ExportData is the top-level JSON structure for export/import.
type ExportData struct {
	Version          int                  `json:"version"`
	ExportedAt       time.Time            `json:"exported_at"`
	Learnings        []ExportLearning     `json:"learnings"`
	PersonaOverrides []ExportPersonaTrait `json:"persona_overrides,omitempty"`
}

// ExportLearning is a single learning in the export format.
type ExportLearning struct {
	Category           string    `json:"category"`
	Content            string    `json:"content"`
	Project            string    `json:"project,omitempty"`
	Source             string    `json:"source"`
	Confidence         float64   `json:"confidence,omitempty"`
	EmotionalIntensity float64   `json:"emotional_intensity,omitempty"`
	SessionFlavor      string    `json:"session_flavor,omitempty"`
	CreatedAt          time.Time `json:"created_at"`

	// V2 metadata — carries what makes a restored learning retrievable again.
	Context            string   `json:"context,omitempty"`
	Domain             string   `json:"domain,omitempty"`
	TriggerRule        string   `json:"trigger_rule,omitempty"`
	Attribution        string   `json:"attribution,omitempty"`
	Entities           []string `json:"entities,omitempty"`
	Actions            []string `json:"actions,omitempty"`
	Keywords           []string `json:"keywords,omitempty"`
	AnticipatedQueries []string `json:"anticipated_queries,omitempty"`

	// Runtime state that a restore used to drop. Additive and omitempty, so an
	// older yesmem reads a file carrying them unchanged and a file without them
	// still imports. exportVersion stays 2: a bump would make the deployed
	// daemon reject every export this build writes.
	//
	// ExpiresAt is the column that decides whether a restored learning is ever
	// visible again (every query filters `expires_at IS NULL OR expires_at > ?`).
	//
	// ValidUntil carries a TTL as well, and it is not only a supersession
	// tombstone any more: cmd_volatile bounds a volatile learning with it while
	// leaving superseded_by at NULL, and that value now survives the store open
	// (YM-348). Dropping it here would turn a bounded learning into a permanent one
	// on the first export/import cycle.
	Importance int        `json:"importance,omitempty"`
	TaskType   string     `json:"task_type,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	ValidUntil *time.Time `json:"valid_until,omitempty"`
	HitCount   int        `json:"hit_count,omitempty"`
	FailCount  int        `json:"fail_count,omitempty"`
}

// ExportPersonaTrait is a persona override in the export format.
type ExportPersonaTrait struct {
	Dimension  string `json:"dimension"`
	TraitKey   string `json:"trait_key"`
	TraitValue string `json:"trait_value"`
}

// nonRecoverableWhere selects active learnings whose source cannot be rebuilt
// from the session archive. The export query and the junction loader share it so
// both see the same row set. An imported row is the exception that proves the
// rule: its llm_extracted source was observed in another installation, so the
// local archive cannot rebuild it and a later export has to carry it.
const nonRecoverableWhere = `l.superseded_by IS NULL
	  AND l.source IS NOT NULL AND l.source <> ''
	  AND (l.source <> 'llm_extracted' OR l.origin_tool = 'import')`

// GetNonRecoverableLearnings returns active learnings that cannot be re-extracted
// from JSONL, together with their V2 metadata. Everything except a locally
// extracted llm_extracted row qualifies: that one can be rebuilt from the
// session archive, while a new source is included here by default instead of
// silently dropped. A learning without a source is left out — InsertLearning
// would restore it as llm_extracted, so it would vanish from the next export.
func (s *Store) GetNonRecoverableLearnings() ([]*models.Learning, error) {
	rows, err := s.readerDB().Query(`
		SELECT l.id, l.session_id, l.category, l.content, l.project, l.source, l.confidence,
		       l.emotional_intensity, l.session_flavor, l.created_at,
		       COALESCE(l.context, ''), COALESCE(l.domain, ''), COALESCE(l.trigger_rule, ''),
		       COALESCE(l.attribution, ''), COALESCE(l.importance, 0), COALESCE(l.task_type, ''),
		       l.expires_at, l.valid_until, COALESCE(l.hit_count, 0), COALESCE(l.fail_count, 0)
		FROM learnings l
		WHERE ` + nonRecoverableWhere + `
		ORDER BY l.created_at ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query non-recoverable: %w", err)
	}
	defer rows.Close()

	var result []*models.Learning
	for rows.Next() {
		l := &models.Learning{}
		var sessionID, project, source, flavor, createdAt, expiresAt, validUntil *string
		if err := rows.Scan(&l.ID, &sessionID, &l.Category, &l.Content, &project,
			&source, &l.Confidence, &l.EmotionalIntensity, &flavor, &createdAt,
			&l.Context, &l.Domain, &l.TriggerRule, &l.Attribution,
			&l.Importance, &l.TaskType, &expiresAt, &validUntil, &l.HitCount, &l.FailCount); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if sessionID != nil {
			l.SessionID = *sessionID
		}
		if project != nil {
			l.Project = *project
		}
		if source != nil {
			l.Source = *source
		}
		if flavor != nil {
			l.SessionFlavor = *flavor
		}
		if createdAt != nil {
			l.CreatedAt = parseTime(*createdAt)
		}
		if expiresAt != nil {
			t := parseTime(*expiresAt)
			l.ExpiresAt = &t
		}
		if validUntil != nil {
			t := parseTime(*validUntil)
			l.ValidUntil = &t
		}
		result = append(result, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("scan non-recoverable: %w", err)
	}

	if err := s.loadJunctionDataBatch(result); err != nil {
		return nil, err
	}
	return result, nil
}

// loadJunctionDataBatch fills entities, actions, keywords and anticipated
// queries for every learning in four statements. Loading them per learning cost
// four statements each, so exporting a real store issued tens of thousands of
// queries.
func (s *Store) loadJunctionDataBatch(learnings []*models.Learning) error {
	if len(learnings) == 0 {
		return nil
	}
	byID := make(map[int64]*models.Learning, len(learnings))
	for _, l := range learnings {
		byID[l.ID] = l
	}

	joins := []struct {
		table string
		dst   func(*models.Learning) *[]string
	}{
		{"learning_entities", func(l *models.Learning) *[]string { return &l.Entities }},
		{"learning_actions", func(l *models.Learning) *[]string { return &l.Actions }},
		{"learning_keywords", func(l *models.Learning) *[]string { return &l.Keywords }},
		{"learning_anticipated_queries", func(l *models.Learning) *[]string { return &l.AnticipatedQueries }},
	}
	for _, j := range joins {
		// The table name comes from the literal list above, never from input.
		rows, err := s.readerDB().Query(`SELECT j.learning_id, j.value FROM ` + j.table + ` j
			JOIN learnings l ON l.id = j.learning_id
			WHERE ` + nonRecoverableWhere)
		if err != nil {
			return fmt.Errorf("load %s: %w", j.table, err)
		}
		for rows.Next() {
			var id int64
			var value string
			if err := rows.Scan(&id, &value); err != nil {
				rows.Close()
				return fmt.Errorf("scan %s: %w", j.table, err)
			}
			if l, ok := byID[id]; ok {
				*j.dst(l) = append(*j.dst(l), value)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("scan %s: %w", j.table, err)
		}
	}
	return nil
}

// ExportLearnings writes non-recoverable learnings + persona overrides to a JSON
// file and returns the number of learnings written.
func (s *Store) ExportLearnings(path string) (int, error) {
	learnings, err := s.GetNonRecoverableLearnings()
	if err != nil {
		return 0, err
	}

	export := ExportData{
		Version:    exportVersion,
		ExportedAt: time.Now(),
	}

	for _, l := range learnings {
		export.Learnings = append(export.Learnings, ExportLearning{
			Category:           l.Category,
			Content:            l.Content,
			Project:            l.Project,
			Source:             l.Source,
			Confidence:         l.Confidence,
			EmotionalIntensity: l.EmotionalIntensity,
			SessionFlavor:      l.SessionFlavor,
			CreatedAt:          l.CreatedAt,
			Context:            l.Context,
			Domain:             l.Domain,
			TriggerRule:        l.TriggerRule,
			Attribution:        l.Attribution,
			Entities:           l.Entities,
			Actions:            l.Actions,
			Keywords:           l.Keywords,
			AnticipatedQueries: l.AnticipatedQueries,
			Importance:         l.Importance,
			TaskType:           l.TaskType,
			ExpiresAt:          l.ExpiresAt,
			ValidUntil:         l.ValidUntil,
			HitCount:           l.HitCount,
			FailCount:          l.FailCount,
		})
	}

	// Export user_override persona traits. Superseded rows stay behind: an
	// import reactivates what it reads, and a deliberately retired override
	// must not come back to life.
	rows, err := s.readerDB().Query(`
		SELECT dimension, trait_key, trait_value
		FROM persona_traits
		WHERE source = 'user_override' AND superseded = FALSE
		ORDER BY trait_key ASC
	`)
	if err != nil {
		return 0, fmt.Errorf("query persona overrides: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var pt ExportPersonaTrait
		if err := rows.Scan(&pt.Dimension, &pt.TraitKey, &pt.TraitValue); err == nil {
			export.PersonaOverrides = append(export.PersonaOverrides, pt)
		}
	}

	data, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return 0, fmt.Errorf("write export: %w", err)
	}
	// WriteFile applies its perm only when it creates the file: an export
	// overwriting one an earlier yesmem wrote world-readable has to be tightened
	// explicitly, otherwise the whole memory corpus stays readable by others.
	if err := os.Chmod(path, 0o600); err != nil {
		return 0, fmt.Errorf("tighten export permissions: %w", err)
	}
	return len(export.Learnings), nil
}

// importLearningSources lists the provenance classes this build writes, plus the
// legacy ones still present in live data. The import path is the one place where
// a source string arrives from a file the daemon did not author: a class outside
// this set cannot be restored faithfully and would let a file mint a trust level
// no code path here knows about.
var importLearningSources = map[string]bool{
	"user_stated": true, "agreed_upon": true, "claude_suggested": true,
	"llm_extracted": true, "llm_extracted_session": true,
	"hook_auto_learned": true, "docs_extracted": true, "system_captured": true,
	"fork": true, "consolidated": true,
	"auto_correct_proposal": true, "auto_correct_accepted": true,
	"auto_correct_proposal_accepted": true,
}

// importSource returns the provenance an imported row may carry. An empty source
// keeps InsertLearning's llm_extracted default; an unknown class keeps its
// content but not its claim and is stored as claude_suggested, the neutral level
// (no decay floor, no [User] label, no persona feed).
func importSource(source string) string {
	if source == "" || importLearningSources[source] {
		return source
	}
	return "claude_suggested"
}

// distrustOutranking lists the provenance classes that outrank claude_suggested
// in the trust chain user_stated > agreed_upon > claude_suggested > llm_extracted,
// plus hook_auto_learned: that class arms the hook block list and carries a higher
// ranking boost than claude_suggested, so a file must not claim it either. Only
// these are lowered by ImportOptions.DistrustFileSources: a lower class keeps its
// smaller claim, so distrusting a file never raises a row.
var distrustOutranking = map[string]bool{
	"user_stated": true, "agreed_upon": true, "hook_auto_learned": true,
}

// distrustSource caps a file-supplied claim at claude_suggested.
func distrustSource(source string) string {
	if distrustOutranking[source] {
		return "claude_suggested"
	}
	return source
}

// importImportance keeps a file from setting a rank no local write produces.
// importance is the exponent of both ranking and supersession resistance
// (TrustScore, importanceBoost), so an imported 1000000 would be un-supersedable.
// 0 stays "unset": InsertLearning then derives the value from the source.
func importImportance(v int) int {
	if v < 0 {
		return 0
	}
	if v > 5 {
		return 5
	}
	return v
}

// reclaimImportCounters keeps a file from writing negative counters.
func reclaimImportCounters(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// ImportOptions carries the switches of a file import. The zero value is the
// documented restore contract: the file's provenance is taken at face value.
type ImportOptions struct {
	// DistrustFileSources caps the provenance claim a file may make:
	// user_stated, agreed_upon and hook_auto_learned arrive as claude_suggested
	// (no decay floor, no [User] label, no persona feed) and the file's persona
	// overrides are skipped, because an import from an untrusted source must not
	// author user-owned directives. The cap is one-way and per row: a later
	// faithful import of the same content is a duplicate and does not raise it.
	DistrustFileSources bool
}

// ImportLearnings reads a JSON export file and inserts non-duplicate learnings,
// trusting the file's own provenance (see ImportOptions).
// Returns (imported, skipped, error).
func (s *Store) ImportLearnings(path string) (int, int, error) {
	return s.ImportLearningsWithOptions(path, ImportOptions{})
}

// ImportLearningsWithOptions is ImportLearnings with explicit switches.
// Returns (imported, skipped, error).
func (s *Store) ImportLearningsWithOptions(path string, opts ImportOptions) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read file: %w", err)
	}

	var export ExportData
	if err := json.Unmarshal(data, &export); err != nil {
		return 0, 0, fmt.Errorf("unmarshal: %w", err)
	}
	if export.Version > exportVersion {
		return 0, 0, fmt.Errorf("export version %d is newer than this yesmem understands (max %d)",
			export.Version, exportVersion)
	}

	var imported, skipped int
	for _, el := range export.Learnings {
		// Check for duplicate (same content + category + project)
		var count int
		err := s.readerDB().QueryRow(`
			SELECT COUNT(*) FROM learnings
			WHERE content = ? AND category = ? AND COALESCE(project, '') = ?
			  AND superseded_by IS NULL
		`, el.Content, el.Category, el.Project).Scan(&count)
		if err != nil {
			return imported, skipped, fmt.Errorf("check dup: %w", err)
		}
		if count > 0 {
			skipped++
			continue
		}

		// An empty created_at (a v1 export, or a row whose created_at never got
		// set) would make the row invisible to every created_at-ordered query, so
		// fall back to the import time.
		createdAt := el.CreatedAt
		if createdAt.IsZero() {
			createdAt = time.Now()
		}

		source := importSource(el.Source)
		if opts.DistrustFileSources {
			source = distrustSource(source)
		}
		_, err = s.InsertLearning(&models.Learning{
			Category:           el.Category,
			Content:            el.Content,
			Project:            el.Project,
			Source:             source,
			Confidence:         el.Confidence,
			EmotionalIntensity: el.EmotionalIntensity,
			SessionFlavor:      el.SessionFlavor,
			CreatedAt:          createdAt,
			ExpiresAt:          el.ExpiresAt,
			ValidUntil:         el.ValidUntil,
			Context:            el.Context,
			Domain:             el.Domain,
			TriggerRule:        el.TriggerRule,
			Attribution:        el.Attribution,
			Entities:           el.Entities,
			Actions:            el.Actions,
			Keywords:           el.Keywords,
			AnticipatedQueries: el.AnticipatedQueries,
			Importance:         importImportance(el.Importance),
			TaskType:           el.TaskType,
			HitCount:           reclaimImportCounters(el.HitCount),
			FailCount:          reclaimImportCounters(el.FailCount),
			// An imported row was observed in another installation, never here.
			// The marker keeps the hook block list (fail_count >= 5 on a
			// hook_auto_learned gotcha) unarmable from a file.
			OriginTool: "import",
			ModelUsed:  "import",
		})
		if err != nil {
			return imported, skipped, fmt.Errorf("insert: %w", err)
		}
		imported++
	}

	// Import persona overrides. A user_override trait is user-owned: full
	// confidence keeps it above the directive's 0.4 gate (the zero value made
	// exported overrides invisible after import), and the source keeps the
	// storage guard protecting it from every automatic path. The file is not
	// authored by this daemon: a value that differs from the local explicit
	// statement is skipped instead of replacing it, and a rejected write is
	// reported through the skip count rather than as a silent success.
	// Import persona overrides. A user_override trait is user-owned: full
	// confidence keeps it above the directive's 0.4 gate (the zero value made
	// exported overrides invisible after import), and the source keeps the
	// storage guard protecting it from every automatic path. The file is not
	// authored by this daemon: a value that differs from the local explicit
	// statement is skipped instead of replacing it, and a rejected write is
	// reported through the skip count rather than as a silent success.
	//
	// No dimension allowlist here. The persona layer deliberately keeps every
	// user_override regardless of its dimension label (directiveTraits, internal
	// /daemon/persona.go): a legacy dimension such as "general" is still the
	// user's own rule. set_persona only gates creation, and a live store carries
	// such legacy overrides, so filtering on import would make a restore drop
	// rules the local store still honours. Prompt safety comes from
	// validateTraitText in UpsertPersonaTrait instead.
	for _, pt := range export.PersonaOverrides {
		// A user_override trait is user-owned and feeds the persona directive:
		// the one artifact a file must not author when its provenance is
		// distrusted. Counted as skipped, like a rejected local write.
		if opts.DistrustFileSources {
			skipped++
			continue
		}
		dim, key := normalizeTraitKey(pt.Dimension, pt.TraitKey)
		// Look at the row regardless of its superseded flag: a retired local
		// override still represents the user's explicit statement, and reviving
		// it with the file's value is exactly what the export filter prevents.
		var existingSource, existingValue string
		lookupErr := s.readerDB().QueryRow(
			`SELECT source, trait_value FROM persona_traits
			 WHERE user_id = 'default' AND dimension = ? AND trait_key = ?`,
			dim, key).Scan(&existingSource, &existingValue)
		if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
			return imported, skipped, fmt.Errorf("check persona override: %w", lookupErr)
		}
		if lookupErr == nil && existingSource == "user_override" && existingValue != pt.TraitValue {
			skipped++
			continue
		}
		if err := s.UpsertPersonaTrait(&models.PersonaTrait{
			UserID:        "default",
			Dimension:     pt.Dimension,
			TraitKey:      pt.TraitKey,
			TraitValue:    pt.TraitValue,
			Confidence:    1.0,
			Source:        "user_override",
			EvidenceCount: 1,
		}); err != nil {
			skipped++
		}
	}

	return imported, skipped, nil
}
