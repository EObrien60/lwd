package web

import (
	"encoding/json"
	"net/http"

	"lwd/internal/client"
	"lwd/internal/reconciler"
)

// nodeRequest is the wire shape for POST /api/nodes.
type nodeRequest struct {
	Name     string `json:"name"`
	SSHHost  string `json:"ssh_host"`
	MeshAddr string `json:"mesh_addr"`
	AgentURL string `json:"agent_url"`
	Pool     string `json:"pool"`
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
	if err := s.client.AddNode(r.Context(), req.Name, req.SSHHost, req.MeshAddr, req.AgentURL, req.Pool); err != nil {
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

// normalizeEvacuateResult normalizes a reconciler.EvacuateResult's nil
// Moved/Skipped/Failed slices to empty, matching every other list-shaped
// /api response (handleApps, handleNodes, handleHealth, handlePools) — so
// the frontend can call .length on them unconditionally.
func normalizeEvacuateResult(res reconciler.EvacuateResult) reconciler.EvacuateResult {
	if res.Moved == nil {
		res.Moved = []string{}
	}
	if res.Skipped == nil {
		res.Skipped = []string{}
	}
	if res.Failed == nil {
		res.Failed = []reconciler.EvacFailure{}
	}
	return res
}

// handleNodeDrain proxies the daemon's POST /nodes/{name}/drain: cordon the
// node (excluding it from future scheduler placement) THEN evacuate every
// scheduler-placed surface off it onto some other fitting node, rendering
// the resulting reconciler.EvacuateResult (moved/skipped/failed).
func (s *Server) handleNodeDrain(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.client.Drain(r.Context(), name)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeEvacuateResult(res))
}

// handleNodeEvacuate proxies the daemon's POST /nodes/{name}/evacuate: move
// every scheduler-placed surface off the node without changing its
// schedulable bit, rendering the same reconciler.EvacuateResult shape as
// drain.
func (s *Server) handleNodeEvacuate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	res, err := s.client.Evacuate(r.Context(), name)
	if err != nil {
		writeClientErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, normalizeEvacuateResult(res))
}

// handleNodeUncordon proxies the daemon's POST /nodes/{name}/uncordon:
// clears the node's cordon, making it eligible for scheduler placement
// again. It never touches anything already deployed on it.
func (s *Server) handleNodeUncordon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.client.Uncordon(r.Context(), name); err != nil {
		writeClientErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePools proxies the daemon's GET /pools: every pool with a registered
// node in it (plus "default", always present), and the count of nodes in
// each. It carries no secret values, so — like every other /api route — it's
// safe behind session auth alone.
func (s *Server) handlePools(w http.ResponseWriter, r *http.Request) {
	pools, err := s.client.Pools(r.Context())
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if pools == nil {
		pools = []client.Pool{}
	}
	writeJSON(w, http.StatusOK, pools)
}
