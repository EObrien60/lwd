package web

import (
	"encoding/json"
	"net/http"

	"lwd/internal/client"
)

// nodeRequest is the wire shape for POST /api/nodes.
type nodeRequest struct {
	Name     string `json:"name"`
	SSHHost  string `json:"ssh_host"`
	MeshAddr string `json:"mesh_addr"`
	AgentURL string `json:"agent_url"`
}

// handleNodes proxies the daemon's GET /nodes: every registered node plus its
// live transport and reachability.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.client.Nodes(r.Context())
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if nodes == nil {
		nodes = []client.NodeStatus{}
	}
	writeJSON(w, http.StatusOK, nodes)
}

// handleNodeAdd proxies the daemon's POST /nodes: register (or update) a
// node. agent_url is optional.
func (s *Server) handleNodeAdd(w http.ResponseWriter, r *http.Request) {
	var req nodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// TODO(P11a Task 8): thread pool
	if err := s.client.AddNode(r.Context(), req.Name, req.SSHHost, req.MeshAddr, req.AgentURL, ""); err != nil {
		writeClientErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleNodeRemove proxies the daemon's DELETE /nodes/{name}.
func (s *Server) handleNodeRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.client.RemoveNode(r.Context(), name); err != nil {
		writeClientErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
