package node

import "testing"

// TestNewAgentNode_NoClientWideTimeout is Finding 1's regression test: an
// http.Client.Timeout bounds an ENTIRE exchange, including a streaming body,
// so a nonzero one would abort long-running SaveImage/LoadImage transfers
// (docker save|load'd images with no registry, see reconciler.
// ensureImageOnNode) partway through. This file lives in package node
// (rather than the external node_test package used by agent_test.go) so it
// can reach AgentNode's unexported client field directly — the simplest,
// most direct way to pin down "no blanket timeout", short of an integration
// test that would need to run for longer than the old 60s cap to prove
// anything.
func TestNewAgentNode_NoClientWideTimeout(t *testing.T) {
	an := NewAgentNode("http://127.0.0.1:0", "tok")
	if an.client == nil {
		t.Fatal("client is nil")
	}
	if an.client.Timeout != 0 {
		t.Fatalf("client.Timeout = %v, want 0 (unbounded; per-call ctx governs lifetime instead)", an.client.Timeout)
	}
}
