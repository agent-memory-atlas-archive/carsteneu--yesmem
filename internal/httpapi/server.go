package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// RPCHandler abstracts the daemon's request handler to avoid import cycles.
// daemon.Handler implements this implicitly via its Handle method.
type RPCHandler interface {
	Handle(req RPCRequest) RPCResponse
}

// RPCRequest mirrors daemon.Request without importing daemon.
type RPCRequest struct {
	Method string
	Params map[string]any
}

// RPCResponse mirrors daemon.Response without importing daemon.
type RPCResponse struct {
	Result json.RawMessage
	Error  string
}

// Server is the HTTP API for OpenClaw integration.
type Server struct {
	handler   RPCHandler
	authToken string
	logger    *log.Logger
	httpSrv   *http.Server
}

// New creates an HTTP API server with daemon handler for RPC pass-through.
func New(handler RPCHandler, listen, authToken string, logger *log.Logger) *Server {
	s := &Server{
		handler:   handler,
		authToken: authToken,
		logger:    logger,
	}

	mux := http.NewServeMux()

	// Health — no auth required
	mux.HandleFunc("GET /health", s.handleHealth)

	// Assemble endpoint — requires auth
	mux.HandleFunc("POST /api/assemble", BearerAuthFunc(authToken, s.handleAssemble))

	// Analyze-turn endpoint — requires auth
	mux.HandleFunc("POST /api/analyze-turn", BearerAuthFunc(authToken, s.handleAnalyzeTurn))

	// Ingest endpoints — require auth
	mux.HandleFunc("POST /api/ingest", BearerAuthFunc(authToken, s.handleIngest))
	mux.HandleFunc("POST /api/ingest-history", BearerAuthFunc(authToken, s.handleIngestHistory))

	// RPC pass-through — requires auth
	mux.HandleFunc("POST /api/rpc/{method}", BearerAuthFunc(authToken, s.handleRPC))

	s.httpSrv = &http.Server{
		Addr:         listen,
		Handler:      LocalhostOnly(RequestLogger(logger, mux)),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	return s
}

// httpRPCMethods is the allowlist of daemon methods POST /api/rpc/{method}
// forwards. An open pass-through made the daemon's whole method table reachable
// over HTTP — including execute_cap (runs code on the host), remove_docs,
// set_config and send_to (steers another agent). Memory reads plus scratchpad
// coordination (read, write, append, list) are what this transport exists for;
// deletion, memory writes, code execution and configuration stay on the local
// socket. Adding a name here is a deliberate decision.
// Deliberately excluded, even though they are reads: code navigation
// (get_code_snippet, get_file_symbols, graph_traverse, search_code_index ...)
// reads paths the caller names, agent inspection (list_agents, get_agent)
// exposes process state, and get_config/get_proxy_state expose daemon state.
// The daemon's handlerAdapter keeps its own httpDeniedMethods deny list as the
// inner gate (internal/daemon/daemon.go, httpDeniedMethods); this list is the
// outer one, so a method denied there should never be listed here.
var httpRPCMethods = map[string]bool{
	"ping": true,

	// Memory reads.
	"search":                          true,
	"hybrid_search":                   true,
	"deep_search":                     true,
	"vector_search":                   true,
	"query_facts":                     true,
	"related_to_file":                 true,
	"get_coverage":                    true,
	"get_learnings":                   true,
	"get_learnings_since":             true,
	"get_session":                     true,
	"get_session_start":               true,
	"get_session_flavors_since":       true,
	"get_session_flavors_for_session": true,
	"get_pulse_learnings_since":       true,
	"get_compacted_stubs":             true,
	"expand_context":                  true,
	"list_projects":                   true,
	"project_summary":                 true,
	"get_project_profile":             true,
	"get_persona":                     true,
	"get_self_feedback":               true,
	"get_pins":                        true,
	"get_plan":                        true,
	"get_active_gaps":                 true,
	"get_repl_pattern_suggestion":     true,
	"get_skill_content":               true,
	"get_caps":                        true,
	"get_active_caps":                 true,
	"list_cap_proposals":              true,
	"get_rules_block":                 true,
	"get_docs_hint":                   true,
	"docs_search":                     true,
	"list_docs":                       true,
	"list_trigger_extensions":         true,
	"contextual_docs":                 true,
	"index_status":                    true,

	// Memory write is NOT here: remember takes its trust level from the request
	// and force_supersede retires an existing learning, so over this transport a
	// token holder could stamp content as user_stated or retire real user
	// knowledge. That write belongs on the local socket.

	// Scratchpad coordination between agents. Delete is NOT here: it removes
	// every row of a caller-named project plus its assets, and this list excludes
	// deletion (remove_docs) for the same reason.
	"scratchpad_read":   true,
	"scratchpad_write":  true,
	"scratchpad_append": true,
	"scratchpad_list":   true,
}

// handleRPC is a generic pass-through for the daemon RPC methods listed in
// httpRPCMethods.
func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	method := r.PathValue("method")
	if !httpRPCMethods[method] {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error": fmt.Sprintf("rpc method %q is not available over the HTTP API", method),
		})
		return
	}

	var params map[string]any
	if err := json.NewDecoder(r.Body).Decode(&params); err != nil {
		params = map[string]any{}
	}

	resp := s.handler.Handle(RPCRequest{
		Method: method,
		Params: params,
	})

	w.Header().Set("Content-Type", "application/json")
	if resp.Error != "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": resp.Error})
		return
	}
	w.Write(resp.Result)
}

// Serve starts listening. Blocks until error or shutdown.
func (s *Server) Serve() error {
	s.logger.Printf("HTTP API listening on %s", s.httpSrv.Addr)
	return s.httpSrv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// handleHealth returns daemon status (no auth required).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}
