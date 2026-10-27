package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/carsteneu/yesmem/internal/models"
)

// ── YM-319 (Los 77): Cap-Katalog, REPL-Hinweis, Profilwahl ──────────────────

const los56fCapsJSON = `[{"id":1,"project":"alpha","source":"user_stated","meta":{"cap_name":"reddit_fetch","cap_description":"Reddit","cap_scripts":[]}}]`
const los56fSuggestionJSON = `{"pattern":{"id":99,"project":"alpha","shape_hash":"deadbeefcafebabe","first_cmd_example":"sqlite3 caps.db SELECT FROM cap_reddit__posts","matched_cap":"reddit_fetch","count":7,"dismiss_count":0}}`

// The REPL-pattern suggestion exists for the Claude Code REPL: it names a cap
// function the model can call directly and the MCP tools cap_search and
// dismiss_repl_pattern. An opencode session has no REPL and no bare cap
// function; it reaches a cap through execute_cap with an fn argument. The hint
// therefore sends it after a construct that does not exist in its dialect.
func TestLos56fReplPatternSuggestionIsClaudeOnly(t *testing.T) {
	tests := []struct {
		name        string
		profile     models.PromptProfile
		wantSuggest bool
	}{
		{name: "claude repl", profile: models.ProfileClaude, wantSuggest: true},
		{name: "opencode code mode", profile: models.ProfileOpencode, wantSuggest: false},
		{name: "codex mcp tools", profile: models.ProfileCodex, wantSuggest: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var suggested bool
			queryFn := func(method string, params map[string]any) (json.RawMessage, error) {
				switch method {
				case "get_active_caps":
					return json.RawMessage(los56fCapsJSON), nil
				case "get_repl_pattern_suggestion":
					suggested = true
					return json.RawMessage(los56fSuggestionJSON), nil
				}
				return nil, errors.New("unexpected: " + method)
			}
			req := map[string]any{
				"messages": []any{map[string]any{"role": "user", "content": "do the thing"}},
			}
			if !injectCapabilitiesTurnImpl(req, "tid", "", tc.profile, queryFn, nil, nil) {
				t.Fatalf("inject returned false")
			}
			if suggested != tc.wantSuggest {
				t.Errorf("REPL suggestion requested = %v, want %v", suggested, tc.wantSuggest)
			}
			msgs := req["messages"].([]any)
			content, _ := msgs[len(msgs)-1].(map[string]any)["content"].(string)
			if strings.Contains(content, "REPL-Pattern") != tc.wantSuggest {
				t.Errorf("last user message carries the REPL hint = %v, want %v: %s",
					strings.Contains(content, "REPL-Pattern"), tc.wantSuggest, content)
			}
		})
	}
}

// Claude Code and opencode can both appear on one request: a proxy chain or a
// wrapper forwards the Claude session header while the opencode headers are
// still on the request. anthropicThreadID lets the Claude header win, so the
// thread id names a Claude Code session. The prompt profile used to be chosen
// from the opencode header alone, so that request got the opencode dialect
// (no eval bootstrap, execute_cap catalog) on a Claude thread, which then
// cached the wrong catalog under the Claude thread key.
func TestLos56fProfileFollowsTheResolvedThread(t *testing.T) {
	backendURL, forwarded := los33Upstream(t)
	s := los26aHandlerServer(t, backendURL)
	los33DaemonSocket(t, s, func(method string, params map[string]any) string {
		if method == "get_active_caps" {
			return los56fCapsJSON
		}
		return "null"
	})

	req := los33MessagesRequest(los33UserTurn("claude-opus-4-6"), map[string]string{
		"X-Claude-Code-Session-Id": "ses_los56f_cc",
		"x-opencode-session":       "ses_los56f_oc",
	})
	rec := httptest.NewRecorder()
	s.handleMessages(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := forwarded()
	if body == "" {
		t.Fatal("backend received no body")
	}
	// The Claude header owns the thread, so the Claude dialect has to follow it.
	if !strings.Contains(body, "eval(result.code)") {
		t.Error("the Claude thread got the opencode catalog: the profile did not follow the resolved thread")
	}
}

// The catalog sits on the byte-stable header prefix, so its rendering must not
// depend on the order the daemon happens to return the caps in: a reordered
// active set with identical content would miss Anthropic's prompt cache on
// every turn. This property moved here when the dead <caps-active> renderer
// (its former test) was removed.
func TestLos56fCapabilitiesCatalogIsOrderIndependent(t *testing.T) {
	mk := func(name string) CapInjection {
		return CapInjection{Name: name, Description: name + " desc"}
	}
	az := renderCapabilitiesCatalog([]CapInjection{mk("alpha"), mk("bravo"), mk("charlie")})
	za := renderCapabilitiesCatalog([]CapInjection{mk("charlie"), mk("bravo"), mk("alpha")})
	if az != za {
		t.Errorf("catalog must be order-independent\nAZ: %q\nZA: %q", az, za)
	}
	ia, ib, ic := strings.Index(az, "alpha"), strings.Index(az, "bravo"), strings.Index(az, "charlie")
	if ia < 0 || ib < 0 || ic < 0 || !(ia < ib && ib < ic) {
		t.Errorf("catalog rows are not in name order: alpha@%d bravo@%d charlie@%d", ia, ib, ic)
	}
}

// The OpenAI parity path is the opencode/codex wire: there is no Claude request
// on it, so the execute_cap catalog belongs there for every profile. The render
// had no test at all, which is how the parity path could have started speaking
// the activate_cap dialect unnoticed.
func TestLos56fOpenAIParityInjectsExecuteCapCatalog(t *testing.T) {
	backendURL, forwarded := los33Upstream(t)
	s := los26aHandlerServer(t, backendURL)
	los33DaemonSocket(t, s, func(method string, params map[string]any) string {
		if method == "get_active_caps" {
			return los56fCapsJSON
		}
		return "null"
	})

	body := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-opencode-session", "ses_los56f_parity")
	rec := httptest.NewRecorder()
	s.handleOpenAICompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	sent := forwarded()
	if sent == "" {
		t.Fatal("backend received no body")
	}
	if !strings.Contains(sent, "execute_cap") {
		t.Error("the parity path must name execute_cap for the opencode catalog")
	}
	if strings.Contains(sent, "mcp__yesmem__activate_cap") {
		t.Error("the parity path must not carry the Claude REPL bootstrap")
	}
	if !strings.Contains(sent, "reddit_fetch") {
		t.Error("the catalog must list the active cap")
	}
}
