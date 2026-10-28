package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/carsteneu/yesmem/internal/codescan"
	"github.com/carsteneu/yesmem/internal/storage"
	"github.com/carsteneu/yesmem/internal/wikidir"
	"github.com/carsteneu/yesmem/internal/wikirender"
)

const wikiTickInterval = 5 * time.Minute

// hotTierAge: hot if latest session is within this window. Older = cold tier.
const hotTierAge = 24 * time.Hour

// coldTierRenderInterval: cold-tier projects render at most this often.
// Slightly under 24h so a tick in the nightly window re-renders them.
const coldTierRenderInterval = 20 * time.Hour

// wikiSnapshot tracks what the last wiki render saw, so we skip re-renders when nothing changed.
type wikiSnapshot struct {
	LearningCount  int    `json:"learnings_count"`
	MaxCreated     string `json:"max_created"` // max(created_at) of active learnings
	ContentSig     int64  `json:"content_sig"` // summed length of the rendered text columns
	SessionCount   int    `json:"sessions_count"`
	MaxSession     string `json:"max_session"`     // max(started_at) of sessions
	MaxSessionEnd  string `json:"max_session_end"` // max(ended_at) of sessions
	SessionMsgSum  int64  `json:"session_msg_sum"` // summed message_count of sessions
	FileCount      int    `json:"files_count"`
	MaxFileTouch   string `json:"max_file_touched"` // max(last_touched) of file_coverage
	FileSessionSum int64  `json:"file_session_sum"` // summed session_count of file_coverage
}

func startWikiTicker(ctx context.Context, store *storage.Store) {
	scanner := codescan.NewCachedScanner(codescan.NewCBMScanner()).WithStore(store)
	go func() {
		ticker := time.NewTicker(wikiTickInterval)
		defer ticker.Stop()
		home, _ := os.UserHomeDir()
		outRoot := wikidir.Root(filepath.Join(home, ".claude", "yesmem"))
		runWikiOnce(ctx, store, outRoot, scanner)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runWikiOnce(ctx, store, outRoot, scanner)
			}
		}
	}()
}

func runWikiOnce(ctx context.Context, store *storage.Store, outRoot string, scanner *codescan.CachedScanner) {
	projects, err := activeProjects(ctx, store)
	if err != nil {
		log.Printf("wiki-tick: list projects: %v", err)
		return
	}

	// One shared naming pass: same-basename projects must not render into the
	// same directory, and a directory already owned by a project stays its own.
	names := wikidir.AssignNames(outRoot, projects)

	for _, p := range projects {
		dir := store.ResolveProjectPath(p)
		name, ok := names[p]
		if !ok {
			log.Printf("wiki-tick: %q has no usable wiki directory name — skipped", p)
			continue
		}
		out := filepath.Join(outRoot, name)

		// Cold-tier throttle: projects with no session in the last hotTierAge
		// render at most once per coldTierRenderInterval. Hot-tier projects
		// render every tick (subject to the content-change check below).
		if !projectTierHot(ctx, store, p) && !coldTierDueForRender(out) {
			continue
		}

		// Skip when nothing changed since last render.
		if !wikiNeedsRender(ctx, store, p, out) {
			continue
		}

		// Record ownership before writing: a colliding project that renders
		// later must find this directory taken. A failed write does not prove
		// the directory is ours: it is only rendered into when its marker
		// already names this project, otherwise it may hold another project's
		// wiki (YM-366).
		if err := wikidir.WriteMarker(out, p); err != nil {
			log.Printf("wiki-tick: mark %q: %s", p, wikiErrorText(err))
			if wikidir.MarkerProject(out) != p {
				continue
			}
		}

		// Only scan code when we're actually going to render and it's a git repo.
		var graph *codescan.CodeGraph
		var graphMs int64
		if dir != "" && isLikelyGitRepo(dir) {
			gStart := time.Now()
			if sr, err := scanner.Scan(dir); err == nil {
				graph = scanner.GetCachedGraph(dir)
				if graph == nil {
					graph = codescan.BuildCodeGraph(sr)
				}
			} else {
				log.Printf("wiki-tick: %q scan failed, rendering without code graph: %s", p, wikiErrorText(err))
			}
			graphMs = time.Since(gStart).Milliseconds()
		}

		r, err := wikirender.Render(ctx, wikirender.RenderConfig{
			Project:   p,
			OutputDir: out,
			Store:     store,
			CodeGraph: graph,
			Quiet:     true,
		})
		if err != nil {
			log.Printf("wiki-tick: render %q: %s", p, wikiErrorText(err))
			continue
		}
		log.Printf("wiki-tick: %q learnings=%d topics=%d files=%d sessions=%d load=%dms compute=%dms write=%dms tpl=%dms graph=%dms total=%dms skipped=%d",
			r.Project, r.Learnings, r.Topics, r.Files, r.Sessions,
			r.LoadMs, r.ComputeMs, r.WriteMs, r.TplMs, graphMs, r.DurationMs, r.SkippedWrites)

		// Persist snapshot so we can skip next tick if nothing changed.
		writeWikiSnapshot(ctx, store, p, out)
	}
}

// wikiErrorText renders a write error of the wiki tick on one line. The path it
// carries is built from the project name, which a client fills, and %v of an
// *os.PathError prints that path unquoted: a newline in the name would end the
// daemon log line and pass the rest off as its own entry (YM-332, the same shape
// as archive.errWithPath).
func wikiErrorText(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return fmt.Sprintf("%s %q: %v", pathErr.Op, pathErr.Path, pathErr.Err)
	}
	var linkErr *os.LinkError
	if errors.As(err, &linkErr) {
		return fmt.Sprintf("%s %q %q: %v", linkErr.Op, linkErr.Old, linkErr.New, linkErr.Err)
	}
	// No path to quote: collapse the whitespace and drop every remaining format
	// character, so neither a line end nor an invisible override reaches the log.
	text := safeAgentText(strings.Join(strings.Fields(err.Error()), " "))
	return strings.Map(func(r rune) rune {
		if unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, text)
}

// wikiNeedsRender returns false if the wiki is up-to-date (learnings unchanged since last render).
func wikiNeedsRender(ctx context.Context, store *storage.Store, project, outDir string) bool {
	wikiIndex := filepath.Join(outDir, "index.md")
	if _, err := os.Stat(wikiIndex); os.IsNotExist(err) {
		return true // never rendered
	}

	// A directory that is not marked as this project's own holds another
	// project's content (or predates the markers): render so the wiki matches
	// its owner.
	if wikidir.MarkerProject(outDir) != project {
		return true
	}

	snap := loadWikiSnapshot(outDir)
	if snap == nil {
		return true // no snapshot = render
	}

	// Check if learnings changed
	lc, maxCreated, contentSig, err := learningSignal(ctx, store, project)
	if err != nil {
		return true // can't determine, render to be safe
	}

	if lc != snap.LearningCount || maxCreated != snap.MaxCreated || contentSig != snap.ContentSig {
		return true
	}

	// Check if sessions changed. A finished session can still grow (message_count)
	// or be closed (ended_at) after the last render; MAX(started_at) alone misses both.
	sessCount, maxSess, maxSessEnd, msgSum, err := sessionSignal(ctx, store, project)
	if err != nil {
		return true
	}
	if sessCount != snap.SessionCount || maxSess != snap.MaxSession ||
		maxSessEnd != snap.MaxSessionEnd || msgSum != snap.SessionMsgSum {
		return true
	}

	// Check if the file list changed. It carries no session of its own, so the
	// session signal above cannot see it.
	fcCount, fcTouched, fcSessions, err := fileCoverageSignal(ctx, store, project)
	if err != nil {
		return true
	}
	if fcCount != snap.FileCount || fcTouched != snap.MaxFileTouch || fcSessions != snap.FileSessionSum {
		return true
	}

	return false // nothing changed
}

func loadWikiSnapshot(outDir string) *wikiSnapshot {
	data, err := os.ReadFile(filepath.Join(outDir, ".wiki-snapshot.json"))
	if err != nil {
		return nil
	}
	var snap wikiSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil
	}
	return &snap
}

// learningSignal returns the change signal for a project's active learnings:
// the row count, the newest creation timestamp and a digest over the columns
// the wiki renders as text. learnings has no updated_at column, so an in-place
// edit of an existing learning is only visible through a change in total
// content length — an edit that keeps every length identical stays invisible.
// The counters (hit/inject/use_count) are deliberately left out: they move on
// every request and would turn the render throttle into a render-every-tick.
func learningSignal(ctx context.Context, store *storage.Store, project string) (count int, maxCreated string, contentSig int64, err error) {
	err = store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MAX(created_at), ''),
		       COALESCE(SUM(LENGTH(COALESCE(content,'')) + LENGTH(COALESCE(category,''))
		         + LENGTH(COALESCE(task_type,'')) + LENGTH(COALESCE(domain,''))
		         + LENGTH(COALESCE(trigger_rule,'')) + LENGTH(COALESCE(context,''))), 0)
		FROM learnings WHERE project=? AND superseded_by IS NULL`, project,
	).Scan(&count, &maxCreated, &contentSig)
	return count, maxCreated, contentSig, err
}

func writeWikiSnapshot(ctx context.Context, store *storage.Store, project, outDir string) {
	snap := wikiSnapshot{}
	snap.LearningCount, snap.MaxCreated, snap.ContentSig, _ = learningSignal(ctx, store, project)
	snap.SessionCount, snap.MaxSession, snap.MaxSessionEnd, snap.SessionMsgSum, _ = sessionSignal(ctx, store, project)
	snap.FileCount, snap.MaxFileTouch, snap.FileSessionSum, _ = fileCoverageSignal(ctx, store, project)

	data, _ := json.Marshal(snap)
	os.WriteFile(filepath.Join(outDir, ".wiki-snapshot.json"), data, 0644)
}

// sessionSignal returns the change signal for the sessions the wiki renders. The
// renderer collects its session ids from the project's learnings: the session
// pages load the newest 200 of them, the file pages list them without a cap
// (wikirender.render.go, queries.go). The signal therefore covers the whole set
// of distinct sessions that own an active learning, deliberately uncapped.
//
// A session without a learning appears on no page, and counting it made the
// signal a superset that forced a re-render on every tick of every hot project
// (YM-266 / DEV-DT43).
//
// The wiki renders started_at, ended_at and message_count per session, so all
// three feed the signal: the session can be extended or closed long after it
// started (YM-198).
func sessionSignal(ctx context.Context, store *storage.Store, project string) (count int, maxStarted, maxEnded string, msgSum int64, err error) {
	err = store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(MAX(started_at), ''),
		       COALESCE(MAX(ended_at), ''),
		       COALESCE(SUM(message_count), 0)
		FROM (
			SELECT s.id, s.started_at, s.ended_at, s.message_count
			FROM sessions s
			JOIN learnings l ON l.session_id = s.id
			WHERE l.project = ? AND l.superseded_by IS NULL
			GROUP BY s.id
		)`, project,
	).Scan(&count, &maxStarted, &maxEnded, &msgSum)
	return count, maxStarted, maxEnded, msgSum, err
}

// fileCoverageSignal returns the change signal for the file list. Those pages
// render session_count and last_touched per file out of file_coverage, which the
// indexer fills from every indexed session, learning or not, so the session
// signal cannot cover it (YM-266).
func fileCoverageSignal(ctx context.Context, store *storage.Store, project string) (count int, maxTouched string, sessionSum int64, err error) {
	err = store.DB().QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(MAX(last_touched), ''), COALESCE(SUM(session_count), 0)
		FROM file_coverage WHERE project = ?`, project,
	).Scan(&count, &maxTouched, &sessionSum)
	return count, maxTouched, sessionSum, err
}

func activeProjects(ctx context.Context, store *storage.Store) ([]string, error) {
	rows, err := store.DB().QueryContext(ctx, `
		SELECT DISTINCT project FROM learnings
		WHERE project IS NOT NULL AND project != '' AND superseded_by IS NULL
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		if !isLiveProjectPath(p) {
			continue
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// isLiveProjectPath returns false for /tmp/* paths (transient opencode session
// dirs) and absolute paths that no longer exist on disk (removed worktrees).
// Returns true for relative paths and stat errors other than IsNotExist so we
// fail open on transient FS issues rather than silently dropping projects.
func isLiveProjectPath(p string) bool {
	if strings.HasPrefix(p, "/tmp/") {
		return false
	}
	if p == "" || p[0] != '/' {
		return true
	}
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return false
	}
	return true
}

// projectTierHot returns true if the project's most recent session is within
// hotTierAge. Projects with no sessions are treated as hot so brand-new
// projects (learnings but no sessions yet) still get a wiki page on the next
// tick.
func projectTierHot(ctx context.Context, store *storage.Store, project string) bool {
	last := projectLastActiveAt(ctx, store, project)
	if last.IsZero() {
		return true
	}
	return time.Since(last) <= hotTierAge
}

func projectLastActiveAt(ctx context.Context, store *storage.Store, project string) time.Time {
	var ts string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COALESCE(MAX(started_at), '') FROM sessions WHERE project_short = ?`, project,
	).Scan(&ts); err != nil || ts == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

// coldTierDueForRender returns true when a cold-tier project should render.
// Decision is based on .wiki-snapshot.json mtime: a snapshot younger than
// coldTierRenderInterval means the previous render is still fresh. Returns
// true when no snapshot exists (first render) or stat fails.
func coldTierDueForRender(outDir string) bool {
	info, err := os.Stat(filepath.Join(outDir, ".wiki-snapshot.json"))
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) >= coldTierRenderInterval
}
