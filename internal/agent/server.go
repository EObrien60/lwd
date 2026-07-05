// Package agent implements the lwd-agent HTTP server: a thin, authed
// wrapper over a local node.Node. It performs no orchestration of its own —
// every route decodes a request, calls straight through to the underlying
// Node, and encodes the result.
package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"

	"lwd/internal/node"
)

// Server exposes a node.Node's primitives over HTTP.
type Server struct {
	node  node.Node
	token string
}

// NewServer returns a Server that delegates every request to n, gated by an
// "Authorization: Bearer <token>" header matching token (except /healthz).
func NewServer(n node.Node, token string) *Server {
	return &Server{node: n, token: token}
}

// Handler returns the full HTTP handler for lwd-agent.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+node.PathHealthz, s.handleHealthz)
	mux.HandleFunc("GET "+node.PathReady, s.handleReady)

	mux.HandleFunc("POST "+node.PathEnsureImage, s.handleEnsureImage)
	mux.HandleFunc("POST "+node.PathImagePresent, s.handleImagePresent)
	mux.HandleFunc("POST "+node.PathEnsureNetwork, s.handleEnsureNetwork)
	mux.HandleFunc("POST "+node.PathConnectNetwork, s.handleConnectNetwork)
	mux.HandleFunc("POST "+node.PathRun, s.handleRun)
	mux.HandleFunc("POST "+node.PathRemove, s.handleRemove)
	mux.HandleFunc("POST "+node.PathList, s.handleList)
	mux.HandleFunc("POST "+node.PathHealth, s.handleHealth)
	mux.HandleFunc("POST "+node.PathContainerHealth, s.handleContainerHealth)

	mux.HandleFunc("GET "+node.PathLogs, s.handleLogs)
	mux.HandleFunc("GET "+node.PathSave, s.handleSave)
	mux.HandleFunc("POST "+node.PathLoad, s.handleLoad)

	return authMiddleware(s.token, mux)
}

// writeJSON writes v as a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// writeErr writes an node.ErrorResponse with the given status code.
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, node.ErrorResponse{Error: err.Error()})
}

// decodeJSON decodes r's body into v, writing a 400 node.ErrorResponse and
// reporting false on failure.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return false
	}
	return true
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if err := s.node.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleReady is a trivial authenticated liveness/readiness check: reaching
// this handler at all means authMiddleware already accepted the caller's
// bearer token (PathReady is the one path NOT exempted from auth), so a bare
// 200 is sufficient — unlike handleHealthz, it does not consult s.node.
// AgentNode.Ping uses this endpoint (with its token) rather than /healthz so
// that transport selection (internal/node.RegistryResolver.buildTransport)
// can distinguish "agent reachable with a working token" from "agent
// reachable" and fall back to ssh on a bad token.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleEnsureImage(w http.ResponseWriter, r *http.Request) {
	var req node.EnsureImageRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.node.EnsureImage(r.Context(), req.Ref); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleImagePresent(w http.ResponseWriter, r *http.Request) {
	var req node.ImagePresentRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	present, err := s.node.ImagePresent(r.Context(), req.Ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, node.ImagePresentResponse{Present: present})
}

func (s *Server) handleEnsureNetwork(w http.ResponseWriter, r *http.Request) {
	var req node.EnsureNetworkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.node.EnsureNetwork(r.Context(), req.Name); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleConnectNetwork(w http.ResponseWriter, r *http.Request) {
	var req node.ConnectNetworkRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.node.ConnectContainerToNetwork(r.Context(), req.ContainerID, req.Network); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req node.RunRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	c, err := s.node.RunContainer(r.Context(), req.Spec)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, node.RunResponse{Container: c})
}

func (s *Server) handleRemove(w http.ResponseWriter, r *http.Request) {
	var req node.RemoveRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.node.RemoveContainer(r.Context(), req.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	var req node.ListRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	list, err := s.node.ListContainers(r.Context(), req.Labels)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, node.ListResponse{Containers: list})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	var req node.HealthCheckRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.node.Health(r.Context(), req.Container, req.Health); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

func (s *Server) handleContainerHealth(w http.ResponseWriter, r *http.Request) {
	var req node.ContainerHealthRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	state, dockerHealth, err := s.node.ContainerHealth(r.Context(), req.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, node.ContainerHealthResponse{State: state, DockerHealth: dockerHealth})
}

// handleLogs streams a container's logs to the response, flushing after each
// chunk so a follow=true request delivers output incrementally.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	follow, _ := strconv.ParseBool(r.URL.Query().Get("follow"))

	rc, err := s.node.ContainerLogs(r.Context(), id, follow)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rc.Close()

	w.WriteHeader(http.StatusOK)
	streamCopy(w, rc)
}

// handleSave streams a tar of the requested image to the response.
func (s *Server) handleSave(w http.ResponseWriter, r *http.Request) {
	ref := r.URL.Query().Get("ref")

	rc, err := s.node.SaveImage(r.Context(), ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	defer rc.Close()

	w.WriteHeader(http.StatusOK)
	streamCopy(w, rc)
}

// handleLoad reads the raw request body as a tar stream and loads it.
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	if err := s.node.LoadImage(r.Context(), r.Body); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, struct{}{})
}

// streamCopy copies rc to w, flushing after each write when w supports it,
// so callers see streamed output incrementally rather than buffered.
func streamCopy(w http.ResponseWriter, rc io.Reader) {
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}
