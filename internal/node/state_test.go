package node

import (
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestParsePSJSONLines(t *testing.T) {
	out := []byte(`{"Name":"p-web-1","Service":"web","State":"running","Health":"healthy","Status":"Up 3 minutes"}
{"Name":"p-worker-1","Service":"worker","State":"exited","Health":"","Status":"Exited (1)"}
`)
	got, err := parsePS(out)
	if err != nil {
		t.Fatal(err)
	}
	want := []Container{
		{Name: "p-web-1", Service: "web", State: "running", Health: "healthy", Status: "Up 3 minutes"},
		{Name: "p-worker-1", Service: "worker", State: "exited", Status: "Exited (1)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v", got)
	}
}

func TestParsePSJSONArray(t *testing.T) {
	got, err := parsePS([]byte(`[{"Name":"a","Service":"web","State":"running"}]`))
	if err != nil || len(got) != 1 || got[0].Service != "web" {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestParsePSEmpty(t *testing.T) {
	for _, in := range []string{"", "\n", "[]"} {
		got, err := parsePS([]byte(in))
		if err != nil || len(got) != 0 {
			t.Errorf("%q: got %+v, %v", in, got, err)
		}
	}
}

func TestParsePSGarbage(t *testing.T) {
	if _, err := parsePS([]byte("not json")); err == nil {
		t.Fatal("expected error")
	}
}

func TestStateRoundTripAndHistoryCap(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Live != nil || len(st.History) != 0 {
		t.Fatalf("fresh state not empty: %+v", st)
	}
	st.Live = &Live{Deployment: 3, Release: 2, Project: "lwd-a-b-d3", TLS: "internal",
		Ports: map[string]int{"web": 20000}, Domains: map[string][]string{"web": {"a.example.com"}}, Ready: map[string]string{"web": "/"}}
	for i := 1; i <= 25; i++ {
		st.record(Attempt{Deployment: int64(i), Release: 1, Status: "failed", Phase: "ready"})
	}
	if err := st.save(path); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", info.Mode())
	}
	back, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Live, st.Live) {
		t.Errorf("live = %+v", back.Live)
	}
	if len(back.History) != historyLimit || back.History[0].Deployment != 25 || back.History[19].Deployment != 6 {
		t.Errorf("history not newest-first capped at %d: first=%d len=%d", historyLimit, back.History[0].Deployment, len(back.History))
	}
}

func TestAllocatePortsLowestFreeSkippingUsed(t *testing.T) {
	a := &portAllocator{min: 20000, max: 20010, free: func(p int) bool { return p != 20002 }}
	used := map[int]bool{20000: true, 20003: true}
	got, err := a.allocate([]string{"web", "api", "admin"}, used)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"web": 20001, "api": 20004, "admin": 20005}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	// Reserved ports stay taken until released, so a concurrent candidate in
	// another app-env cannot be handed the same port.
	again, _ := a.allocate([]string{"web"}, used)
	if again["web"] != 20006 {
		t.Fatalf("reserved port reused: %v", again)
	}
	a.release(got)
	again2, _ := a.allocate([]string{"web"}, used)
	if again2["web"] != 20001 {
		t.Fatalf("released port not reused: %v", again2)
	}
}

func TestAllocatePortsExhausted(t *testing.T) {
	a := &portAllocator{min: 20000, max: 20001, free: func(int) bool { return true }}
	if _, err := a.allocate([]string{"a", "b", "c"}, nil); err == nil {
		t.Fatal("expected exhaustion error")
	}
	// A failed allocation must not leak reservations.
	if got, err := a.allocate([]string{"a", "b"}, nil); err != nil || got["a"] != 20000 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestPortBindable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot bind loopback in this sandbox")
	}
	port := l.Addr().(*net.TCPAddr).Port
	if portBindable(port) {
		t.Error("port in use reported bindable")
	}
	l.Close()
	if !portBindable(port) {
		t.Error("released port reported unbindable")
	}
}
