package daemon

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// scratchpadAssetsDir returns the assets directory of a scratchpad project.
// project is a caller value and may be an absolute repository path, so the guard
// proves containment below <dataDir>/projects instead of rejecting separators:
// ".." segments used to resolve outside it, and the delete path removes the
// result recursively (OPS-68). The proof is lexical by design: it stops caller
// strings, not a symlink planted inside the projects root, which would need the
// daemon's own write access.
func (h *Handler) scratchpadAssetsDir(project string) (string, error) {
	if h.dataDir == "" {
		return "", fmt.Errorf("scratchpad project %q: data dir is empty", project)
	}
	root := filepath.Join(h.dataDir, "projects")
	dir := filepath.Join(root, project, "assets")
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", fmt.Errorf("scratchpad project %q: %v", project, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("scratchpad project %q escapes the projects directory", project)
	}
	return dir, nil
}

// handleScratchpadWrite upserts a scratchpad section for a project.
func (h *Handler) handleScratchpadWrite(params map[string]any) Response {
	project, _ := params["project"].(string)
	section, _ := params["section"].(string)
	content, _ := params["content"].(string)

	if project == "" {
		return errorResponse("project required")
	}
	if section == "" {
		return errorResponse("section required")
	}

	owner := h.resolveSessionID(params, "owner")

	if err := h.store.ScratchpadWrite(project, section, content, owner); err != nil {
		return errorResponse(err.Error())
	}

	// Update heartbeat for running agents that write to scratchpad
	if owner != "" {
		h.store.AgentUpdateBySessionID(owner, map[string]any{
			"heartbeat_at": time.Now().Format(time.RFC3339),
		})
	}

	// Ensure assets directory exists for this project. A project the guard
	// refuses still stores its section, so the refusal is logged rather than
	// failing the write; nothing is created outside the projects root.
	if assetsDir, err := h.scratchpadAssetsDir(project); err != nil {
		log.Printf("[scratchpad] assets dir for %q refused: %v", project, err)
	} else if err := os.MkdirAll(assetsDir, 0755); err != nil {
		log.Printf("[scratchpad] assets dir for %q: %v", project, err)
	}

	return jsonResponse(map[string]any{
		"status":  "ok",
		"project": project,
		"section": section,
	})
}

// handleScratchpadAppend appends content to an existing scratchpad section.
// Unlike scratchpad_write (which replaces), this reads the current content,
// appends the new content with a divider, and writes back. If the section
// doesn't exist yet, it behaves like scratchpad_write (creates it).
func (h *Handler) handleScratchpadAppend(params map[string]any) Response {
	project, _ := params["project"].(string)
	section, _ := params["section"].(string)
	content, _ := params["content"].(string)

	if project == "" {
		return errorResponse("project required")
	}
	if section == "" {
		return errorResponse("section required")
	}
	if content == "" {
		return errorResponse("content required")
	}

	owner := h.resolveSessionID(params, "owner")

	// Read existing content
	existing, err := h.store.ScratchpadRead(project, section)
	if err != nil {
		return errorResponse(err.Error())
	}

	var combined string
	if len(existing) > 0 && existing[0].Content != "" {
		combined = existing[0].Content + "\n\n---\n\n" + content
	} else {
		combined = content
	}

	if err := h.store.ScratchpadWrite(project, section, combined, owner); err != nil {
		return errorResponse(err.Error())
	}

	// Update heartbeat for running agents that write to scratchpad
	if owner != "" {
		h.store.AgentUpdateBySessionID(owner, map[string]any{
			"heartbeat_at": time.Now().Format(time.RFC3339),
		})
	}

	return jsonResponse(map[string]any{
		"status":   "ok",
		"project":  project,
		"section":  section,
		"appended": true,
	})
}

// handleScratchpadRead returns scratchpad sections for a project.
// If section param is provided, only that section is returned.
func (h *Handler) handleScratchpadRead(params map[string]any) Response {
	project, _ := params["project"].(string)
	if project == "" {
		return errorResponse("project required")
	}
	section, _ := params["section"].(string)

	sections, err := h.store.ScratchpadRead(project, section)
	if err != nil {
		return errorResponse(err.Error())
	}

	type sectionResult struct {
		Section   string `json:"section"`
		Content   string `json:"content"`
		Owner     string `json:"owner"`
		UpdatedAt string `json:"updated_at"`
	}

	result := make([]sectionResult, 0, len(sections))
	for _, s := range sections {
		result = append(result, sectionResult{
			Section:   s.Section,
			Content:   s.Content,
			Owner:     s.Owner,
			UpdatedAt: s.UpdatedAt,
		})
	}

	return jsonResponse(map[string]any{
		"project":  project,
		"sections": result,
	})
}

// handleScratchpadList returns a summary of projects with scratchpad entries.
// If project param is provided, only that project is returned.
func (h *Handler) handleScratchpadList(params map[string]any) Response {
	project, _ := params["project"].(string)

	projects, err := h.store.ScratchpadList(project)
	if err != nil {
		return errorResponse(err.Error())
	}

	type sectionSummary struct {
		Section   string `json:"section"`
		Owner     string `json:"owner"`
		Size      int    `json:"size"`
		UpdatedAt string `json:"updated_at"`
	}
	type projectResult struct {
		Project      string           `json:"project"`
		SectionCount int              `json:"section_count"`
		LastUpdated  string           `json:"last_updated"`
		Sections     []sectionSummary `json:"sections"`
	}

	result := make([]projectResult, 0, len(projects))
	for _, p := range projects {
		secs := make([]sectionSummary, 0, len(p.Sections))
		for _, s := range p.Sections {
			secs = append(secs, sectionSummary{
				Section:   s.Section,
				Owner:     s.Owner,
				Size:      s.Size,
				UpdatedAt: s.UpdatedAt,
			})
		}
		result = append(result, projectResult{
			Project:      p.Project,
			SectionCount: p.SectionCount,
			LastUpdated:  p.LastUpdated,
			Sections:     secs,
		})
	}

	return jsonResponse(map[string]any{
		"projects": result,
	})
}

// handleScratchpadDelete removes scratchpad entries for a project (or a single section).
// If section is omitted the entire project's scratchpad is deleted, including its assets directory.
func (h *Handler) handleScratchpadDelete(params map[string]any) Response {
	project, _ := params["project"].(string)
	if project == "" {
		return errorResponse("project required")
	}
	section, _ := params["section"].(string)

	// Validated before the rows are touched: an escaping project belongs to no
	// section and must not delete anything.
	var assetsDir string
	if section == "" {
		var err error
		assetsDir, err = h.scratchpadAssetsDir(project)
		if err != nil {
			return errorResponse(err.Error())
		}
	}

	count, err := h.store.ScratchpadDelete(project, section)
	if err != nil {
		return errorResponse(err.Error())
	}

	// Remove assets directory when deleting the whole project scratchpad
	if section == "" {
		_ = os.RemoveAll(assetsDir)
	}

	return jsonResponse(map[string]any{
		"status":        "deleted",
		"deleted_count": count,
	})
}
