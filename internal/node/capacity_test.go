package node

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

func TestParseMeminfo(t *testing.T) {
	r := strings.NewReader("MemTotal:  16384000 kB\nMemAvailable: 8192000 kB\n")
	total, avail, err := parseMeminfo(r)
	if err != nil {
		t.Fatalf("parseMeminfo: %v", err)
	}
	if want := int64(16384000 * 1024); total != want {
		t.Errorf("total = %d, want %d", total, want)
	}
	if want := int64(8192000 * 1024); avail != want {
		t.Errorf("avail = %d, want %d", avail, want)
	}
}

func TestParseMeminfoMissingTotal(t *testing.T) {
	r := strings.NewReader("MemAvailable: 8192000 kB\n")
	if _, _, err := parseMeminfo(r); err == nil {
		t.Fatal("expected error when MemTotal is missing")
	}
}

func TestParseLoadavg1(t *testing.T) {
	r := strings.NewReader("0.50 0.40 0.30 1/234 5678")
	got, err := parseLoadavg1(r)
	if err != nil {
		t.Fatalf("parseLoadavg1: %v", err)
	}
	if got != 0.5 {
		t.Errorf("parseLoadavg1 = %v, want 0.5", got)
	}
}

func TestFakeCapacity(t *testing.T) {
	f := NewFake()
	f.Cap = Capacity{CPUCores: 4, MemAvailable: 1 << 30, Known: true}
	ctx := context.Background()

	got, err := f.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if got != f.Cap {
		t.Errorf("Capacity = %+v, want %+v", got, f.Cap)
	}
	found := false
	for _, c := range f.Calls {
		if c == "Capacity" {
			found = true
		}
	}
	if !found {
		t.Errorf("Calls = %v, want it to contain Capacity", f.Calls)
	}

	f.CapErr = errors.New("boom")
	if _, err := f.Capacity(ctx); err == nil {
		t.Fatal("expected CapErr to be returned")
	}
}

func TestLocalCapacityRuntime(t *testing.T) {
	l, err := NewLocal()
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	ctx := context.Background()

	c, err := l.Capacity(ctx)
	if err != nil {
		t.Fatalf("Capacity: %v", err)
	}
	if runtime.GOOS == "linux" {
		if !c.Known {
			t.Errorf("Known = false on linux, want true")
		}
	} else if c.Known {
		t.Errorf("Known = true on %s, want false", runtime.GOOS)
	}
	if c.CPUCores <= 0 {
		t.Errorf("CPUCores = %d, want > 0", c.CPUCores)
	}
}
