package compose

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

var _ Composer = (*Fake)(nil)

func TestFakeUpRecordsSpec(t *testing.T) {
	f := NewFake()
	spec := UpSpec{
		Project: "lwd-myapp",
		File:    "/apps/myapp/compose.yaml",
		Env:     map[string]string{"FOO": "bar"},
	}

	if err := f.Up(context.Background(), spec); err != nil {
		t.Fatalf("Up: unexpected error: %v", err)
	}

	if !reflect.DeepEqual(f.LastUp, spec) {
		t.Fatalf("LastUp = %+v, want %+v", f.LastUp, spec)
	}

	found := false
	for _, c := range f.Calls {
		if c == "Up:"+spec.Project {
			found = true
		}
	}
	if !found {
		t.Fatalf("Calls = %v, want to contain %q", f.Calls, "Up:"+spec.Project)
	}
}

func TestFakeUpErr(t *testing.T) {
	f := NewFake()
	f.UpErr = errors.New("boom")

	err := f.Up(context.Background(), UpSpec{Project: "p"})
	if !errors.Is(err, f.UpErr) {
		t.Fatalf("Up err = %v, want %v", err, f.UpErr)
	}
}

func TestFakeServiceContainer(t *testing.T) {
	f := NewFake()
	f.ServiceID = "abc123"
	f.ServiceName = "myapp-web-1"

	id, name, err := f.ServiceContainer(context.Background(), "lwd-myapp", "web")
	if err != nil {
		t.Fatalf("ServiceContainer: unexpected error: %v", err)
	}
	if id != "abc123" || name != "myapp-web-1" {
		t.Fatalf("ServiceContainer = (%q, %q), want (%q, %q)", id, name, "abc123", "myapp-web-1")
	}

	found := false
	for _, c := range f.Calls {
		if c == "ServiceContainer:lwd-myapp:web" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Calls = %v, want to contain %q", f.Calls, "ServiceContainer:lwd-myapp:web")
	}
}

func TestFakeServiceContainerErr(t *testing.T) {
	f := NewFake()
	f.ServiceErr = errors.New("no container")

	_, _, err := f.ServiceContainer(context.Background(), "lwd-myapp", "web")
	if !errors.Is(err, f.ServiceErr) {
		t.Fatalf("ServiceContainer err = %v, want %v", err, f.ServiceErr)
	}
}

func TestFakeDown(t *testing.T) {
	f := NewFake()

	if err := f.Down(context.Background(), "lwd-myapp", "/apps/myapp/compose.yaml"); err != nil {
		t.Fatalf("Down: unexpected error: %v", err)
	}

	found := false
	for _, c := range f.Calls {
		if c == "Down:lwd-myapp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Calls = %v, want to contain %q", f.Calls, "Down:lwd-myapp")
	}
}

func TestFakeDownErr(t *testing.T) {
	f := NewFake()
	f.DownErr = errors.New("down failed")

	err := f.Down(context.Background(), "lwd-myapp", "compose.yaml")
	if !errors.Is(err, f.DownErr) {
		t.Fatalf("Down err = %v, want %v", err, f.DownErr)
	}
}
