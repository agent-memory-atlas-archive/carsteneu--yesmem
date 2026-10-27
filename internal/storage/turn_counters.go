package storage

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"github.com/carsteneu/yesmem/internal/models"
)

// IncrementTurnCount atomically increments the turn counter for a project.
// Returns the new count. Creates the row if it doesn't exist.
func (s *Store) IncrementTurnCount(project string) (int64, error) {
	var count int64
	err := s.db.QueryRow(`INSERT INTO turn_counters (project, turn_count, updated_at) VALUES (?, 1, CURRENT_TIMESTAMP) ON CONFLICT(project) DO UPDATE SET turn_count = turn_count + 1, updated_at = CURRENT_TIMESTAMP RETURNING turn_count`, project).Scan(&count)
	return count, err
}

// GetTurnCount returns the current turn count for a project. Returns 0 if not found.
func (s *Store) GetTurnCount(project string) (int64, error) {
	var count int64
	err := s.db.QueryRow(`SELECT turn_count FROM turn_counters WHERE project = ?`, project).Scan(&count)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return count, err
}

// GetTurnCountsBulk returns turn counts for multiple projects in a single query.
// Projects not found return 0.
//
// Reads go through the reader pool: this is a pure SELECT, and the write
// connection is limited to one connection, so reading through it blocks
// whenever a write transaction is open (hooks and briefings read turn counts
// while extraction and maintenance batches write).
func (s *Store) GetTurnCountsBulk(projects []string) (map[string]int64, error) {
	result := make(map[string]int64)
	if len(projects) == 0 {
		return result, nil
	}
	placeholders := make([]string, len(projects))
	args := make([]any, len(projects))
	for i, p := range projects {
		placeholders[i] = "?"
		args[i] = p
	}
	rows, err := s.readerDB().Query(fmt.Sprintf(`SELECT project, turn_count FROM turn_counters WHERE project IN (%s)`, strings.Join(placeholders, ",")), args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var proj string
		var count int64
		if err := rows.Scan(&proj, &count); err != nil {
			return result, err
		}
		result[proj] = count
	}
	return result, rows.Err()
}

// EnrichTurnCounts fills CurrentTurnCount from turn_counters for every learning,
// keyed by the learning's own project (empty project = the global counter, the
// same keying InsertLearningBatch and the hybrid re-rank use). The scoring
// functions read that field; without it TurnBasedDecay is a no-op and the
// project boost stays pinned at its freshest tier.
func (s *Store) EnrichTurnCounts(learnings []models.Learning) {
	if len(learnings) == 0 {
		return
	}

	projects := make(map[string]bool, len(learnings))
	hasGlobal := false
	for _, l := range learnings {
		if l.Project != "" {
			projects[l.Project] = true
		} else {
			hasGlobal = true
		}
	}
	projectList := make([]string, 0, len(projects)+1)
	for p := range projects {
		projectList = append(projectList, p)
	}
	if hasGlobal {
		projectList = append(projectList, "__global__")
	}

	turnCounts := make(map[string]int64, len(projectList))
	// Chunk the IN clause: the driver rejects more than 32766 bound variables,
	// and yesmem keys projects by full path, so the count only grows.
	const chunk = 500
	for start := 0; start < len(projectList); start += chunk {
		end := start + chunk
		if end > len(projectList) {
			end = len(projectList)
		}
		counts, err := s.GetTurnCountsBulk(projectList[start:end])
		if err != nil {
			// Silent failure here would restore exactly the YM-34 symptom (decay
			// pinned at 1.0) with no trace of why.
			log.Printf("[storage] enrich turn counts failed for %d learnings across %d projects: %v", len(learnings), len(projectList), err)
			return
		}
		for project, count := range counts {
			turnCounts[project] = count
		}
	}

	for i := range learnings {
		key := learnings[i].Project
		if key == "" {
			key = "__global__"
		}
		learnings[i].CurrentTurnCount = turnCounts[key]
	}
}
