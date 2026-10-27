package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/carsteneu/yesmem/internal/clustering"
	"github.com/carsteneu/yesmem/internal/embedding"
	"github.com/carsteneu/yesmem/internal/extraction"
	"github.com/carsteneu/yesmem/internal/models"
	"github.com/carsteneu/yesmem/internal/storage"
)

// clusterLearnings groups project learnings by embedding similarity and labels them via LLM.
// Runs as Phase 4.5 between Narratives and Profiles.
// Embeddings are read from the VectorStore — no on-the-fly computation.
// If vecStore is nil or empty, clustering is skipped.
// Operates on one group per repository: the worktree copies of one repository
// form one group, so their learnings land in one cluster set and each cluster is
// labelled once. Uses the cluster fingerprint for change detection.
func ClusterLearnings(store *storage.Store, client extraction.LLMClient, vecStore *embedding.VectorStore) {
	if vecStore == nil || vecStore.Count() == 0 {
		log.Printf("  Clustering: skipped (no embeddings in vector store)")
		return
	}

	projects, err := store.ListProjects()
	if err != nil {
		log.Printf("warn: list projects for clustering: %v", err)
		return
	}

	skipped := 0
	clustered := 0
	for _, p := range storage.GroupByCanonicalProject(projects) {
		if p.Sessions < 3 {
			continue
		}
		// A group whose key is no repository path (a legacy bare name) has no cluster
		// key to write: the writer refuses it (YM-424). Clustering it would relabel the
		// same learnings on every run without ever converging, so it is skipped once.
		if models.ClusterProjectKey(p.ClusterKey) == "" {
			skipped++
			continue
		}
		// Change detection: compare the learning-set fingerprint with the stored hash
		fingerprint, fpErr := store.ClusterChangeFingerprint(p.Canonical)
		if fpErr != nil {
			// Fail open: cluster anyway, but do not store a hash that would make
			// the next run believe the project is up to date.
			log.Printf("warn: cluster fingerprint %s: %v", p.ClusterKey, fpErr)
			fingerprint = ""
		}
		storedHash, _ := store.GetClusterHash(p.ClusterKey)
		if fingerprint != "" && storedHash == fingerprint {
			skipped++
			continue
		}
		if err := clusterProjectLearnings(store, client, vecStore, p.Canonical, p.ClusterKey); err != nil {
			log.Printf("warn: cluster %s: %v", p.ClusterKey, err)
			continue
		}
		if fingerprint != "" {
			store.SetClusterHash(p.ClusterKey, fingerprint)
		}
		clustered++
	}
	if skipped > 0 {
		log.Printf("  Clustering: %d projects clustered, %d skipped (unchanged)", clustered, skipped)
	}
}

// learningBelongsToClusterKey reports whether a learning written in project is a
// member of the repository whose cluster rows live under clusterKey. The scope of
// the learnings query is the canonical basename, so two checkouts named "repo"
// return each other's rows; the cluster rows are keyed per project path, so the
// membership follows the path the learning was written in (worktrees folded onto
// their repository, which is exactly what the key does). A row whose project is a
// legacy short name carries no path, cannot be placed, and stays in the basename
// scope, which is the fallback the cluster key uses for a bare name.
func learningBelongsToClusterKey(project, clusterKey string) bool {
	if project == "" || !filepath.IsAbs(project) || !filepath.IsAbs(clusterKey) {
		return true
	}
	return models.ClusterProjectKey(project) == clusterKey
}

// capClusterable trims the learning set to the clustering budget by taking an
// even stride over the population, ordered by ID. A hit-count head would only
// cluster the learnings that were already surfaced, so the tail never reached
// the three-member floor that distillation and recurrence require.
func capClusterable(learnings []models.Learning, limit int) []models.Learning {
	if limit <= 0 || len(learnings) <= limit {
		return learnings
	}
	sort.Slice(learnings, func(i, j int) bool { return learnings[i].ID < learnings[j].ID })
	step := float64(len(learnings)) / float64(limit)
	sampled := make([]models.Learning, limit)
	for i := 0; i < limit; i++ {
		// Centered grid: anchoring at index 0 would drop the newest tail every
		// time, and the newest learnings are what recurrence looks for.
		sampled[i] = learnings[int((float64(i)+0.5)*step)]
	}
	return sampled
}

// clusterProjectLearnings clusters the learnings of one repository. project is the
// learnings scope (the canonical basename on the learning rows), clusterKey the key
// its cluster rows live under. The two differ whenever two project paths share a
// basename (two checkouts named "repo") or when a directory inside a checkout has
// its own scope; the membership is resolved per learning through
// models.ClusterProjectKey, so a key only ever holds its own scope's learnings.
func clusterProjectLearnings(store *storage.Store, client extraction.LLMClient, vecStore *embedding.VectorStore, project, clusterKey string) error {
	// Get all active learnings for project (excluding narrative, unfinished)
	learnings, err := store.GetActiveLearnings("", project, "", "", 0)
	if err != nil {
		return nil
	}
	if len(learnings) < 5 {
		// Too few to cluster, so no cluster set is the truthful answer, and the rows
		// on record have to match it. Leaving them would keep a set that was built
		// before the membership was narrowed, still serving learnings this
		// repository does not own.
		return store.ReplaceLearningClusters(clusterKey, nil)
	}

	// Filter to clusterworthy categories. recurrence_alert and narrative are
	// derived rows: clustering them would feed meta text back into distillation
	// and shift cluster membership on every alert. Global learnings
	// (canonical_project = '') belong to no repository: GetActiveLearnings returns
	// them for every project, so leaving them in would make one learning a member
	// of each project's cluster set and inflate every cluster by the same rows.
	var clusterable []models.Learning
	for _, l := range learnings {
		if l.CanonicalProject == "" {
			continue
		}
		switch l.Category {
		case "narrative", "unfinished", "strategic", "recurrence_alert":
			continue
		}
		if !learningBelongsToClusterKey(l.Project, clusterKey) {
			continue
		}
		clusterable = append(clusterable, l)
	}

	if len(clusterable) < 5 {
		return store.ReplaceLearningClusters(clusterKey, nil)
	}

	// Cap at 300 to avoid O(n³) freeze with large projects
	const maxClusterable = 300
	if len(clusterable) > maxClusterable {
		log.Printf("  Clustering: capping %d → %d learnings for %s", len(clusterable), maxClusterable, project)
		clusterable = capClusterable(clusterable, maxClusterable)
	}

	// Build documents using pre-computed embeddings from VectorStore
	ctx := context.Background()
	docs := make([]clustering.Document, 0, len(clusterable))
	skipped := 0

	for _, l := range clusterable {
		id := strconv.FormatInt(l.ID, 10)
		emb := vecStore.GetEmbedding(ctx, id)
		if emb == nil {
			skipped++
			continue // Not yet in vector store — will be included next run
		}
		docs = append(docs, clustering.Document{
			ID:      id,
			Content: l.Content,
			Embedding: emb,
			Metadata: map[string]any{
				"category":   l.Category,
				"hit_count":  l.HitCount,
				"created_at": l.CreatedAt,
			},
		})
	}

	if skipped > 0 {
		log.Printf("  Clustering %s: %d with embeddings, %d skipped (not yet indexed)", project, len(docs), skipped)
	}

	if len(docs) < 5 {
		// Not enough with embeddings. The set on record is deliberately KEPT here: what
		// is missing is an embedding, not a learning. Replacing it with an empty set
		// would erase a valid cluster set on every backfill in progress and on every
		// provider change, and the rows would come back one run later with different
		// labels. A membership that really shrank is caught above (len(clusterable) < 5),
		// which is the branch that clears the rows.
		return nil // Not enough with embeddings
	}

	// Cluster with 0.85 threshold, min 2 docs per cluster
	clusters := clustering.AgglomerativeClustering(docs, 0.85)
	clusters = clustering.FilterByMinSize(clusters, 2)

	if len(clusters) == 0 {
		return nil
	}

	// Label clusters via LLM (or simple heuristic if no client)
	var dbClusters []models.LearningCluster
	for i, c := range clusters {
		if i > 0 && client != nil {
			time.Sleep(500 * time.Millisecond) // rate-limit LLM calls
		}
		label := labelCluster(c, client)
		ids := make([]int64, 0, len(c.Documents))
		var totalRecency, totalHits float64
		for _, d := range c.Documents {
			id, _ := strconv.ParseInt(d.ID, 10, 64)
			ids = append(ids, id)
			if created, ok := d.Metadata["created_at"].(time.Time); ok {
				totalRecency += time.Since(created).Hours() / 24
			}
			if hc, ok := d.Metadata["hit_count"].(int); ok {
				totalHits += float64(hc)
			}
		}
		n := float64(len(c.Documents))
		idsJSON, _ := json.Marshal(ids)

		// Confidence: weighted formula — count 30%, recency 40%, hits 30%
		avgRecency := totalRecency / n
		avgHits := totalHits / n
		countScore := math.Min(n/10.0, 1.0)                                         // saturates at 10 learnings
		recencyScore := math.Exp(-avgRecency / 60.0)                                 // half-life ~42 days
		hitScore := math.Min(avgHits/5.0, 1.0)                                       // saturates at 5 hits
		confidence := countScore*0.3 + recencyScore*0.4 + hitScore*0.3
		if confidence > 1.0 {
			confidence = 1.0
		}
		if confidence < 0.05 {
			confidence = 0.05
		}

		dbClusters = append(dbClusters, models.LearningCluster{
			Project:        clusterKey,
			Label:          label,
			LearningCount:  len(c.Documents),
			AvgRecencyDays: avgRecency,
			AvgHitCount:    avgHits,
			Confidence:     confidence,
			LearningIDs:    string(idsJSON),
		})
	}

	return store.ReplaceLearningClusters(clusterKey, dbClusters)
}

// labelCluster generates a short German label for a cluster.
// Uses LLM if available, otherwise falls back to first content snippet.
func labelCluster(c clustering.Cluster, client extraction.LLMClient) string {
	if client == nil || len(c.Documents) == 0 {
		if len(c.Documents) > 0 {
			content := c.Documents[0].Content
			if len(content) > 50 {
				content = content[:50]
			}
			return content
		}
		return "Unnamed cluster"
	}

	// Build content list for LLM
	var contents []string
	for _, d := range c.Documents {
		s := d.Content
		if len(s) > 200 {
			s = s[:200]
		}
		contents = append(contents, "- "+s)
	}

	prompt := "You are a knowledge organizer. Give this cluster of insights a short English label (3-6 words). Only the label, no explanation."
	userMsg := fmt.Sprintf("Insights:\n%s", strings.Join(contents, "\n"))

	label, err := client.Complete(prompt, userMsg)
	if err != nil {
		return c.Documents[0].Content[:min(50, len(c.Documents[0].Content))]
	}
	// Trim quotes and whitespace
	label = trimLabel(label)
	return label
}

func trimLabel(s string) string {
	// Remove surrounding quotes
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		s = s[1 : len(s)-1]
	}
	return strings.TrimSpace(s)
}
