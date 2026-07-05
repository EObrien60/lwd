package web

import (
	"net/http"

	"lwd/internal/reconciler"
)

// handleHealth proxies the daemon's GET /health: the continuous reconciler's
// current point-in-time snapshot (node reachability, edge/router health, and
// per-app self-heal state). It carries no secret values — see
// reconciler.Health's fields — so it's safe to expose to any authenticated
// browser session, same as every other /api route.
//
// Nodes/Apps are normalized to empty (rather than null) slices, like the
// other list-shaped /api responses (handleApps, handleNodes), so the
// frontend can always call .length/.filter on them without a null check.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	h, err := s.client.Health(r.Context())
	if err != nil {
		writeClientErr(w, err)
		return
	}
	if h.Nodes == nil {
		h.Nodes = []reconciler.NodeHealth{}
	}
	if h.Apps == nil {
		h.Apps = []reconciler.AppHealth{}
	}
	writeJSON(w, http.StatusOK, h)
}
