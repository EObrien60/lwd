package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"lwd/internal/router"
)

// historyLimit is how many deploy attempts state.json remembers.
const historyLimit = 20

// State is apps/<app>-<env>/state.json: the node's last-known truth for one
// app-env. It is what a restarted node rebuilds routes from.
type State struct {
	App     string    `json:"app"`
	Env     string    `json:"env"`
	Live    *Live     `json:"live,omitempty"`
	History []Attempt `json:"history"` // newest first
}

// Live describes the deployment currently receiving traffic.
type Live struct {
	Deployment int64               `json:"deployment"`
	Release    int64               `json:"release"`
	Project    string              `json:"project"`
	TLS        string              `json:"tls"`
	Ports      map[string]int      `json:"ports"`   // HTTP service -> loopback host port
	Domains    map[string][]string `json:"domains"` // HTTP service -> routed domains
	Ready      map[string]string   `json:"ready"`   // HTTP service -> readiness path
}

// Attempt is one deploy outcome in State.History.
type Attempt struct {
	Deployment int64  `json:"deployment"`
	Release    int64  `json:"release"`
	Status     string `json:"status"`
	Phase      string `json:"phase"`
	At         string `json:"at"`
}

func loadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &State{}, nil
	}
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &st, nil
}

func (st *State) save(path string) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return router.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

func (st *State) record(a Attempt) {
	st.History = append([]Attempt{a}, st.History...)
	if len(st.History) > historyLimit {
		st.History = st.History[:historyLimit]
	}
}

// Container is one row of `docker compose ps --format json`.
type Container struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	State   string `json:"state"`
	Health  string `json:"health,omitempty"`
	Status  string `json:"status,omitempty"`
}

// parsePS accepts both shapes compose has emitted for `ps --format json`: a
// single JSON array (compose < 2.21) and one object per line (newer).
// encoding/json matches the capitalised compose keys case-insensitively.
func parsePS(out []byte) ([]Container, error) {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil, nil
	}
	if out[0] == '[' {
		var cs []Container
		if err := json.Unmarshal(out, &cs); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
		return cs, nil
	}
	var cs []Container
	dec := json.NewDecoder(bytes.NewReader(out))
	for dec.More() {
		var c Container
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("parse compose ps: %w", err)
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// portAllocator hands out loopback host ports for HTTP services. A port is
// free when no persisted live deployment uses it, no in-flight candidate has
// reserved it, and it can actually be bound right now.
type portAllocator struct {
	min, max int
	free     func(port int) bool // bindability probe; portBindable in production

	mu       sync.Mutex
	reserved map[int]bool
}

// allocate assigns the lowest free ports, in service order, and reserves them
// until release. used holds ports owned by persisted live deployments.
func (a *portAllocator) allocate(services []string, used map[int]bool) (map[string]int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.reserved == nil {
		a.reserved = map[int]bool{}
	}
	out := map[string]int{}
	next := a.min
	for _, s := range services {
		for ; next <= a.max; next++ {
			if !used[next] && !a.reserved[next] && a.free(next) {
				break
			}
		}
		if next > a.max {
			return nil, fmt.Errorf("no free host port in %d-%d", a.min, a.max)
		}
		out[s] = next
		next++
	}
	for _, p := range out {
		a.reserved[p] = true
	}
	return out, nil
}

func (a *portAllocator) release(ports map[string]int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range ports {
		delete(a.reserved, p)
	}
}

func portBindable(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}
