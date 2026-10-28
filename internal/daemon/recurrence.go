package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// recurrenceMinNewLearnings is how many new learnings must accumulate before
// recurrence detection pays for LLM interpretation again.
const recurrenceMinNewLearnings = 50

// recurrenceCursorKey holds the highest learning ID the last detection run saw.
const recurrenceCursorKey = "recurrence_cursor_id"

const recurrencePrompt = `Du bekommst einen Cluster ähnlicher Learnings die wiederholt auftauchen.
Frage: Deuten diese auf ein tieferes Architektur-/Design-Problem hin?

Wenn ja: Beschreibe in 1-2 Sätzen (max 200 Zeichen) was die wahrscheinliche Ursache ist und was geprüft werden sollte.
Wenn nein (z.B. normales Feature-Cluster, Lern-Fortschritt, verschiedene Aspekte eines Themas): antworte NUR mit "kein_problem".`

// recurrenceMu serialises detection runs. Two callers exist (the extraction phase
// and the two-hourly ticker); both read the cursor before either advances it, so
// without the lock they can duplicate alerts and LLM spend.
var recurrenceMu sync.Mutex

// DetectRecurrence finds clusters with recurring patterns and generates alerts.
// Step 1: Heuristic pre-filter (zero cost). Step 2: Haiku interpretation (only candidates).
func DetectRecurrence(store *storage.Store, client extraction.LLMClient) int {
	recurrenceMu.Lock()
	defer recurrenceMu.Unlock()

	maxID, err := store.GetMaxLearningID()
	if err != nil {
		log.Printf("  warn: recurrence gate: %v", err)
		return 0
	}
	cursor := recurrenceCursor(store)
	if cursor > maxID {
		// A cursor above the table is unreachable (restore, pruning): clamping to
		// 0 is the only way detection recovers without manual DB surgery.
		log.Printf("  warn: recurrence cursor %d is above max learning id %d, resetting", cursor, maxID)
		cursor = 0
	}
	newLearnings, err := store.CountLearningsSinceID(cursor)
	if err != nil {
		log.Printf("  warn: recurrence gate: %v", err)
		return 0
	}
	if newLearnings < recurrenceMinNewLearnings {
		log.Printf("[recurrence] skipped: only %d new learnings (need %d)", newLearnings, recurrenceMinNewLearnings)
		return 0
	}

	projects, err := store.ListProjects()
	if err != nil {
		return 0
	}

	var totalAlerts int
	for _, p := range storage.GroupByCanonicalProject(projects) {
		alerts := detectForProject(store, client, p.Canonical, p.ClusterKey)
		totalAlerts += alerts
	}

	// Advance only after a completed pass: a skipped run keeps the cursor so the
	// gate can still be reached once enough learnings accumulate.
	if err := store.SetProxyState(recurrenceCursorKey, strconv.FormatInt(maxID, 10)); err != nil {
		log.Printf("  warn: recurrence cursor: %v", err)
	}
	return totalAlerts
}

// recurrenceCursor returns the highest learning ID the last detection run saw.
func recurrenceCursor(store *storage.Store) int64 {
	raw, err := store.GetProxyState(recurrenceCursorKey)
	if err != nil || raw == "" {
		return 0
	}
	cursor, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || cursor < 0 {
		return 0
	}
	return cursor
}

func detectForProject(store *storage.Store, client extraction.LLMClient, project, clusterKey string) int {
	clusters, err := store.GetLearningClusters(clusterKey)
	if err != nil {
		return 0
	}

	// Load existing recurrence alerts for dedup
	existingAlerts, _ := store.GetLearningsByCategory("recurrence_alert", project, 100)
	alertLabels := make(map[string]bool, len(existingAlerts))
	for _, a := range existingAlerts {
		alertLabels[a.Content] = true
	}

	var alertCount int
	// Verdict keys belong to the current cluster set; keys for membership that no
	// longer exists are dead weight in proxy_state.
	seenVerdicts := make(map[string]bool, len(clusters))
	for _, cluster := range clusters {
		if !isRecurrenceCandidate(cluster) {
			continue
		}

		// Parse learning IDs
		var ids []int64
		if err := json.Unmarshal([]byte(cluster.LearningIDs), &ids); err != nil {
			continue
		}

		// Load active learnings + count distinct sessions
		var learnings []models.Learning
		sessionSet := make(map[string]bool)
		for _, id := range ids {
			l, err := store.GetLearning(id)
			if err != nil || l == nil || l.SupersededBy != nil {
				continue
			}
			learnings = append(learnings, *l)
			if l.SessionID != "" {
				sessionSet[l.SessionID] = true
			}
		}

		if len(learnings) < 3 || len(sessionSet) < 2 {
			continue
		}

		// A dismissed cluster stays dismissed until its membership changes:
		// re-asking the LLM for an unchanged cluster only burns budget. The key is
		// registered before the label dedup so an existing alert does not make the
		// next pass forget the verdict and pay again.
		verdictKey := recurrenceVerdictPrefix(clusterKey) + recurrenceClusterKey(ids)
		seenVerdicts[verdictKey] = true

		// Dedup: skip if alert for this cluster label already exists
		if containsLabel(alertLabels, cluster.Label) {
			continue
		}

		if verdict, _ := store.GetProxyState(verdictKey); verdict == "kein_problem" {
			continue
		}

		// Step 2: LLM interpretation (or template fallback)
		alertText, dismissed := interpretCluster(client, cluster, learnings, len(sessionSet))
		if dismissed {
			if err := store.SetProxyState(verdictKey, "kein_problem"); err != nil {
				log.Printf("  warn: store recurrence verdict: %v", err)
			}
			continue
		}
		if alertText == "" {
			continue
		}

		// Insert as learning. Keep a real project path on the row: path-keyed
		// consumers (attribution, staleness) expect the source project value.
		alertProject := project
		if len(learnings) > 0 && learnings[0].Project != "" {
			alertProject = learnings[0].Project
		}
		newLearning := &models.Learning{
			Content:    alertText,
			Category:   "recurrence_alert",
			Project:    alertProject,
			Source:     "consolidated",
			Importance: 5,
			CreatedAt:  time.Now(),
		}
		if _, err := store.InsertLearning(newLearning); err != nil {
			log.Printf("  warn: insert recurrence alert: %v", err)
			continue
		}

		alertCount++
		log.Printf("  ⚠ Recurrence: %q → %s", cluster.Label, alertText)
	}

	if deleted, err := store.PruneProxyStatePrefix(recurrenceVerdictPrefix(clusterKey), seenVerdicts); err != nil {
		log.Printf("  warn: prune recurrence verdicts for %s: %v", clusterKey, err)
	} else if deleted > 0 {
		log.Printf("  recurrence: dropped %d stale verdicts for %s", deleted, clusterKey)
	}

	return alertCount
}

func isRecurrenceCandidate(c models.LearningCluster) bool {
	return c.LearningCount >= 3 && c.AvgRecencyDays < 14
}

func interpretCluster(client extraction.LLMClient, cluster models.LearningCluster, learnings []models.Learning, sessionCount int) (string, bool) {
	if client == nil {
		return templateAlert(cluster, learnings, sessionCount), false
	}

	// Build context for LLM
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Cluster: %q (%d Learnings, %d Sessions, Ø%.0f Tage alt)\n\n",
		cluster.Label, len(learnings), sessionCount, cluster.AvgRecencyDays))
	for _, l := range learnings {
		sb.WriteString(fmt.Sprintf("- [%s] [%s] %s\n", l.Category, l.CreatedAt.Format(time.DateOnly), l.Content))
	}

	response, err := client.Complete(recurrencePrompt, sb.String())
	if err != nil {
		log.Printf("  warn: recurrence LLM call: %v", err)
		return templateAlert(cluster, learnings, sessionCount), false
	}

	response = strings.TrimSpace(response)
	if strings.Contains(strings.ToLower(response), "kein_problem") {
		return "", true
	}

	return fmt.Sprintf("⚠ %q: %s", cluster.Label, response), false
}

// recurrenceClusterKey identifies a cluster by its membership: the labels are
// LLM output and drift between runs, the learning IDs do not.
func recurrenceClusterKey(ids []int64) string {
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	sum := sha256.Sum256([]byte(fmt.Sprint(sorted)))
	return hex.EncodeToString(sum[:8])
}

// recurrenceProjectToken encodes a cluster key for use inside a proxy_state key.
// A project path may contain the separator itself, and one project's verdicts are
// pruned by string prefix, so an unescaped key would let the prune of project "a"
// delete the verdicts of project "a:b". Replacing the separator (after escaping the
// escape character) keeps the encoded token free of ':' and therefore makes the
// prefix match exact.
func recurrenceProjectToken(project string) string {
	return strings.ReplaceAll(strings.ReplaceAll(project, "%", "%25"), ":", "%3A")
}

// recurrenceVerdictPrefix is the proxy_state prefix holding one project's
// dismissed clusters.
func recurrenceVerdictPrefix(project string) string {
	return "recurrence_verdict:" + recurrenceProjectToken(project) + ":"
}

func templateAlert(cluster models.LearningCluster, learnings []models.Learning, sessionCount int) string {
	days := int(cluster.AvgRecencyDays)
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("Recurring pattern: %q (%d learnings in %d sessions over %d days). Consider architectural fix.",
		cluster.Label, len(learnings), sessionCount, days)
}

func containsLabel(alertSet map[string]bool, label string) bool {
	for content := range alertSet {
		if strings.Contains(content, label) {
			return true
		}
	}
	return false
}
