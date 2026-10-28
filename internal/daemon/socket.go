package daemon

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"time"
)

const sockName = "daemon.sock"

// jobSockName is the socket a sandboxed run is allowed to see. It serves a
// restricted method set (see handleJobSocket), so a job can reach cap_store and
// the LLM bridge without reaching the RPCs that start processes.
const jobSockName = "job.sock"

// chmodSocket is a seam for the tests: a filesystem without chmod support cannot
// be produced on demand, and the degraded start path still has to be proven.
var chmodSocket = os.Chmod

// socketMode reports the mode the socket actually has, for the warning after a
// refused chmod: the error alone does not say whether other accounts can write.
func socketMode(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return "unknown"
	}
	return fmt.Sprintf("%04o", info.Mode().Perm())
}

// SocketPath returns the path to the daemon's Unix socket.
func SocketPath(dataDir string) string {
	return filepath.Join(dataDir, sockName)
}

// JobSocketPath returns the path to the socket a sandboxed run may mount. It sits
// in the runtime dir beside the data dir: the sandbox masks the data dir as a
// whole (YM-276), so a socket inside it would be hidden from the runs that need it.
func JobSocketPath(dataDir string) string {
	return filepath.Join(RuntimeDir(dataDir), jobSockName)
}

// clientSocketPath resolves the socket a CLI client dials. YESMEM_SOCK
// redirects it: a sandboxed run only has the restricted socket mounted, so the
// yesmem binary it calls has to be pointed at that one. The variable can only
// narrow what its own process reaches — the value never grants a path the
// caller could not already use.
func clientSocketPath(dataDir string) string {
	if path := os.Getenv("YESMEM_SOCK"); path != "" {
		return path
	}
	return SocketPath(dataDir)
}

// IsDaemonRunning checks if the daemon socket is reachable.
func IsDaemonRunning(dataDir string) bool {
	conn, err := net.Dial("unix", clientSocketPath(dataDir))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Request is a JSON-RPC style request to the daemon.
type Request struct {
	Method string         `json:"method"`
	Params map[string]any `json:"params,omitempty"`
}

// Response is a JSON-RPC style response from the daemon.
type Response struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// SocketServer listens on a Unix socket and dispatches requests to handlers.
type SocketServer struct {
	listener net.Listener
	handler  func(req Request) Response
}

// NewSocketServer creates a Unix socket server.
func NewSocketServer(dataDir string, handler func(req Request) Response) (*SocketServer, error) {
	return NewSocketServerAt(SocketPath(dataDir), handler)
}

// NewSocketServerAt creates a Unix socket server on an explicit path. The daemon
// runs two: the full one at daemon.sock and the restricted one a sandboxed run
// may mount.
func NewSocketServerAt(sockPath string, handler func(req Request) Response) (*SocketServer, error) {
	// Remove stale socket
	os.Remove(sockPath)

	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", sockPath, err)
	}
	// net.Listen applies the umask, so on a host with umask 002 the socket is
	// group- and world-writable. The client runs as the same user, and a
	// sandboxed job reaches the daemon through its own socket, so owner-only
	// mode costs no reachability and closes the full RPC set to other accounts.
	// A mode that cannot be set is reported, not fatal: filesystems without chmod
	// support would otherwise make the daemon unstartable, and the socket lives in
	// a 0700 data directory (audit 16.10.2026, YM-338).
	if err := chmodSocket(sockPath, 0o600); err != nil {
		log.Printf("cannot restrict socket %s to owner-only: %v (mode stays %s; the data directory is 0700)", sockPath, err, socketMode(sockPath))
	}

	log.Printf("Socket server listening on %s", sockPath)

	return &SocketServer{
		listener: listener,
		handler:  handler,
	}, nil
}

// Serve accepts connections and handles requests. Blocks until Close.
func (s *SocketServer) Serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return // listener closed
		}
		go s.handleConn(conn)
	}
}

// Close stops the socket server.
func (s *SocketServer) Close() error {
	return s.listener.Close()
}

func (s *SocketServer) handleConn(conn net.Conn) {
	defer conn.Close()

	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	// The peer is read once per connection and attached to every request from
	// it, as an int and unconditionally — overwriting whatever the client sent
	// under that key, and writing the "unknown" answer (-1) too: handlers that
	// need to know who is calling (the channel endpoints, see
	// channelInboxSession) must never read a value the caller could choose.
	peer := peerPID(conn)

	for {
		var req Request
		if err := decoder.Decode(&req); err != nil {
			return // client disconnected
		}
		if req.Params == nil {
			req.Params = make(map[string]any, 1)
		}
		req.Params["_peer_pid"] = peer
		resp := s.handler(req)
		if err := encoder.Encode(resp); err != nil {
			return
		}
	}
}

// SocketClient connects to the daemon's Unix socket.
type SocketClient struct {
	conn    net.Conn
	encoder *json.Encoder
	decoder *json.Decoder
}

// Dial connects to the daemon socket.
func Dial(dataDir string) (*SocketClient, error) {
	conn, err := net.Dial("unix", clientSocketPath(dataDir))
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	return &SocketClient{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(conn),
	}, nil
}

// DialTimeout connects to the daemon socket with a timeout.
func DialTimeout(dataDir string, timeout time.Duration) (*SocketClient, error) {
	conn, err := net.DialTimeout("unix", clientSocketPath(dataDir), timeout)
	if err != nil {
		return nil, fmt.Errorf("dial daemon: %w", err)
	}
	return &SocketClient{
		conn:    conn,
		encoder: json.NewEncoder(conn),
		decoder: json.NewDecoder(conn),
	}, nil
}

// Call sends a request and waits for the response.
func (c *SocketClient) Call(method string, params map[string]any) (json.RawMessage, error) {
	req := Request{Method: method, Params: params}
	if err := c.encoder.Encode(req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}

	var resp Response
	if err := c.decoder.Decode(&resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}

	if resp.Error != "" {
		return nil, fmt.Errorf("daemon: %s", resp.Error)
	}
	return resp.Result, nil
}

// SetDeadline sets the underlying connection deadline.
func (c *SocketClient) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// Close closes the connection.
func (c *SocketClient) Close() error {
	return c.conn.Close()
}
