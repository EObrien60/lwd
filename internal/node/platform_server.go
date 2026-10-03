package node

import (
	"context"
	"encoding/json"
	"net/http"

	"lwd/internal/bundle"
)

const maxPlatformBody = 64 << 10

func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPlatformBody)).Decode(v); err != nil {
		return invalidf("decode body: %v", err)
	}
	return nil
}

// Provisioning, backup and restore run to completion even if the controller
// hangs up: a half-provisioned resource is harmless, but a restore that
// stopped an app must also start it again.

func (n *Node) handleProvisionDatabase(w http.ResponseWriter, r *http.Request) {
	var req bundle.DatabaseRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	name := r.PathValue("name")
	if err := n.ProvisionDatabase(context.WithoutCancel(r.Context()), name, req); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": name})
}

func (n *Node) handleProvisionBucket(w http.ResponseWriter, r *http.Request) {
	var req bundle.BucketRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	creds, err := n.ProvisionBucket(context.WithoutCancel(r.Context()), r.PathValue("name"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, creds)
}

func (n *Node) handleBackup(w http.ResponseWriter, r *http.Request) {
	bf, err := n.BackupDatabase(context.WithoutCancel(r.Context()), r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bf)
}

func (n *Node) handleListBackups(w http.ResponseWriter, r *http.Request) {
	bs, err := n.ListBackups(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (n *Node) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req bundle.RestoreRequest
	if err := decodeBody(w, r, &req); err != nil {
		writeErr(w, err)
		return
	}
	res, err := n.RestoreDatabase(context.WithoutCancel(r.Context()), r.PathValue("name"), req)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
