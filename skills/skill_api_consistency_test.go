package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/config"
)

// yesResearchSkillPath is the bundled yesresearch skill (installed to
// ~/.claude/skills/yesresearch/SKILL.md by yesmem setup).
const yesResearchSkillPath = "bundled-skills/yesresearch/SKILL.md"

// yesloopSkillPath is the bundled yesloop skill, the pipeline every loop agent
// is told to follow.
const yesloopSkillPath = "bundled-skills/yesloop/SKILL.md"

// spawnAgentSource is the handler whose params[...] reads define the spawn API.
const spawnAgentSource = "../internal/daemon/handler_agents.go"

// spawnAgentParams derives the parameter set handleSpawnAgent actually reads from
// its source. A hand-maintained copy would keep accepting a documented parameter
// after the daemon dropped it, which is the drift this test exists to prevent.
func spawnAgentParams(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile(spawnAgentSource)
	if err != nil {
		t.Fatalf("read %s: %v", spawnAgentSource, err)
	}
	body := functionBody(string(src), "func (h *Handler) handleSpawnAgent(")
	if body == "" {
		t.Fatalf("%s: handleSpawnAgent not found, the API surface moved", spawnAgentSource)
	}

	names := make(map[string]bool)
	for _, m := range regexp.MustCompile(`params\["([a-z_]+)"\]`).FindAllStringSubmatch(body, -1) {
		names[m[1]] = true
	}
	// callerPidParam reads this one for the caller's terminal window.
	names["_caller_pid"] = true
	if len(names) < 5 {
		t.Fatalf("%s: only %d spawn parameters found, the extraction broke", spawnAgentSource, len(names))
	}
	return names
}

// functionBody returns the source from the signature to the next top-level func.
func functionBody(src, signature string) string {
	i := strings.Index(src, signature)
	if i < 0 {
		return ""
	}
	rest := src[i:]
	if j := strings.Index(rest, "\nfunc "); j >= 0 {
		return rest[:j]
	}
	return rest
}

// TestYesResearchSkillOnlyNamesRealSpawnParams: every yesmem_spawn_agent(...)
// call in the skill may only use parameters the daemon accepts.
func TestYesResearchSkillOnlyNamesRealSpawnParams(t *testing.T) {
	data, err := os.ReadFile(yesResearchSkillPath)
	if err != nil {
		t.Fatalf("read %s: %v", yesResearchSkillPath, err)
	}
	known := spawnAgentParams(t)

	callRe := regexp.MustCompile(`yesmem_spawn_agent\(([^)]*)\)`)
	checked := 0
	for _, call := range callRe.FindAllStringSubmatch(string(data), -1) {
		for _, arg := range strings.Split(call[1], ",") {
			name, _, ok := strings.Cut(strings.TrimSpace(arg), "=")
			if !ok {
				continue
			}
			checked++
			if !known[strings.TrimSpace(name)] {
				t.Errorf("%s names spawn parameter %q, which handleSpawnAgent does not read", yesResearchSkillPath, strings.TrimSpace(name))
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no spawn parameter found, the skill's spawn calls changed shape", yesResearchSkillPath)
	}
}

// TestYesResearchSkillOnlyNamesRealListAgentsParams: list_agents takes project
// only. A status filter in the skill silently returns every row, so a cap check
// that trusts it counts dead agents as live (YM-153 review).
func TestYesResearchSkillOnlyNamesRealListAgentsParams(t *testing.T) {
	data, err := os.ReadFile(yesResearchSkillPath)
	if err != nil {
		t.Fatalf("read %s: %v", yesResearchSkillPath, err)
	}

	callRe := regexp.MustCompile(`list_agents\(([^)]*)\)`)
	checked := 0
	for _, call := range callRe.FindAllStringSubmatch(string(data), -1) {
		for _, arg := range strings.Split(call[1], ",") {
			arg = strings.TrimSpace(arg)
			if arg == "" {
				continue
			}
			checked++
			name, _, _ := strings.Cut(arg, "=")
			name = strings.TrimSpace(name)
			if name != "project" {
				t.Errorf("%s passes %q to list_agents, which reads project only", yesResearchSkillPath, arg)
			}
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no list_agents argument found, the skill's monitoring calls changed shape", yesResearchSkillPath)
	}
}

// TestYesResearchSkillConfigKeysResolve: a set_config key in the skill must pass
// the same normalization the daemon applies, otherwise the documented call fails
// at runtime (max_runtime sits under agents, not at the top level).
func TestYesResearchSkillConfigKeysResolve(t *testing.T) {
	data, err := os.ReadFile(yesResearchSkillPath)
	if err != nil {
		t.Fatalf("read %s: %v", yesResearchSkillPath, err)
	}

	keyRe := regexp.MustCompile(`set_config\("([^"]+)"`)
	checked := 0
	for _, m := range keyRe.FindAllStringSubmatch(string(data), -1) {
		checked++
		if _, err := config.NormalizeSetPath(m[1]); err != nil {
			t.Errorf("%s documents set_config(%q), which the daemon rejects: %v", yesResearchSkillPath, m[1], err)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no set_config key found, the skill's config guidance changed shape", yesResearchSkillPath)
	}
}

// TestYesResearchSkillDoesNotPromiseListAgentsStreamFields: list_agents returns
// the stored agent rows, get_agent enriches them with stream_active and
// subagent_streams. A monitoring rule that reads stream_active off list_agents
// waits for a field that never arrives (YM-153).
func TestYesResearchSkillDoesNotPromiseListAgentsStreamFields(t *testing.T) {
	data, err := os.ReadFile(yesResearchSkillPath)
	if err != nil {
		t.Fatalf("read %s: %v", yesResearchSkillPath, err)
	}

	for i, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(line, "list_agents") || !strings.Contains(line, "stream_active") {
			continue
		}
		if strings.Contains(line, "get_agent") {
			continue
		}
		t.Errorf("%s:%d attributes stream_active to list_agents; list_agents returns the agent rows without stream enrichment, get_agent has the field", yesResearchSkillPath, i+1)
	}
}

// TestYesloopSkillBunTestPathsExist: the proof example names the plugin's test
// directory. Los 18c moved it to opencode-yesmem-v2, and a path that no longer
// exists makes the recommended proof command fail before it proves anything
// (YM-296).
func TestYesloopSkillBunTestPathsExist(t *testing.T) {
	data, err := os.ReadFile(yesloopSkillPath)
	if err != nil {
		t.Fatalf("read %s: %v", yesloopSkillPath, err)
	}

	// Leading flags are skipped, then the first non-space token is the path. A
	// repo path must be relative: an absolute one survives a repo move.
	pathRe := regexp.MustCompile("bun test (?:-{1,2}[A-Za-z-]+(?:=\\S+)? )*([^\\s`]+)")
	checked := 0
	for _, m := range pathRe.FindAllStringSubmatch(string(data), -1) {
		p := m[1]
		if p == "./..." || p == "..." {
			continue // a package pattern, not a path
		}
		if filepath.IsAbs(p) {
			t.Errorf("%s names `bun test %s`: the example must use a repo-relative path", yesloopSkillPath, p)
			continue
		}
		checked++
		if _, err := os.Stat(filepath.Join("..", p)); err != nil {
			t.Errorf("%s names `bun test %s`, which does not exist in the repo: %v", yesloopSkillPath, p, err)
		}
	}
	if checked == 0 {
		t.Fatalf("%s: no bun test path found, the proof example changed shape", yesloopSkillPath)
	}
}
