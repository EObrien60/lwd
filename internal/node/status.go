package node

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lwd/internal/version"
)

// NodeStatus is the body of GET /v1/status.
type NodeStatus struct {
	Hostname      string      `json:"hostname"`
	Version       string      `json:"version"`
	UptimeSeconds int64       `json:"uptime_seconds"` // of the lwd node process
	Load          [3]float64  `json:"load"`           // 1, 5, 15 minute load averages
	Memory        Memory      `json:"memory"`
	Disk          Disk        `json:"disk"` // filesystem holding /
	Caddy         Caddy       `json:"caddy"`
	Apps          []AppStatus `json:"apps"`
	Errors        []string    `json:"errors,omitempty"` // host facts that could not be read
}

type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type Disk struct {
	TotalBytes uint64 `json:"total_bytes"`
	FreeBytes  uint64 `json:"free_bytes"` // available to unprivileged users
}

type Caddy struct {
	State  string `json:"state"` // compose container state, "missing" or "unknown"
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

// AppStatus is one app-env on this node.
type AppStatus struct {
	App        string      `json:"app"`
	Env        string      `json:"env"`
	Live       *Live       `json:"live,omitempty"`
	Last       *Attempt    `json:"last,omitempty"` // most recent deploy attempt
	Containers []Container `json:"containers"`
	Error      string      `json:"error,omitempty"`
}

func (n *Node) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, n.status(r.Context()))
}

func (n *Node) status(ctx context.Context) NodeStatus {
	st := NodeStatus{Version: version.String, UptimeSeconds: int64(time.Since(n.started).Seconds()), Apps: []AppStatus{}}
	fail := func(what string, err error) { st.Errors = append(st.Errors, what+": "+err.Error()) }

	var err error
	if st.Hostname, err = os.Hostname(); err != nil {
		fail("hostname", err)
	}
	if st.Load, err = readLoad(filepath.Join(n.procDir, "loadavg")); err != nil {
		fail("loadavg", err)
	}
	if st.Memory, err = readMemory(filepath.Join(n.procDir, "meminfo")); err != nil {
		fail("meminfo", err)
	}
	var fs syscall.Statfs_t
	if err := syscall.Statfs("/", &fs); err != nil {
		fail("statfs", err)
	} else {
		st.Disk = Disk{TotalBytes: fs.Blocks * uint64(fs.Bsize), FreeBytes: fs.Bavail * uint64(fs.Bsize)}
	}

	st.Caddy = Caddy{State: "unknown"}
	if cs, err := n.ps(ctx, n.systemProject()); err != nil {
		st.Caddy.Error = err.Error()
	} else {
		st.Caddy.State = "missing"
		for _, c := range cs {
			if c.Service == "caddy" {
				st.Caddy.State, st.Caddy.Status = c.State, c.Status
			}
		}
	}

	states, err := n.loadStates()
	if err != nil {
		fail("state", err)
	}
	keys := make([]string, 0, len(states))
	for k := range states {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		s := states[key]
		a := AppStatus{App: s.App, Env: s.Env, Live: s.Live, Containers: []Container{}}
		if len(s.History) > 0 {
			a.Last = &s.History[0]
		}
		if s.Live != nil {
			cs, err := n.ps(ctx, n.liveProject(key, s.Live))
			if err != nil {
				a.Error = err.Error()
			} else if cs != nil {
				a.Containers = cs
			}
		}
		st.Apps = append(st.Apps, a)
	}
	return st
}

func (n *Node) ps(ctx context.Context, p project) ([]Container, error) {
	out, err := n.docker.Run(ctx, p.args("ps", "--all", "--format", "json")...)
	if err != nil {
		return nil, err
	}
	cs, err := parsePS(out)
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].Service < cs[j].Service })
	return cs, err
}

func readLoad(path string) ([3]float64, error) {
	var l [3]float64
	data, err := os.ReadFile(path)
	if err != nil {
		return l, err
	}
	f := strings.Fields(string(data))
	if len(f) < 3 {
		return l, fmt.Errorf("unexpected format %q", data)
	}
	for i := range l {
		if l[i], err = strconv.ParseFloat(f[i], 64); err != nil {
			return l, err
		}
	}
	return l, nil
}

func readMemory(path string) (Memory, error) {
	var m Memory
	data, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			m.TotalBytes = kb * 1024
		case "MemAvailable:":
			m.AvailableBytes = kb * 1024
		}
	}
	if m.TotalBytes == 0 {
		return m, fmt.Errorf("MemTotal not found")
	}
	return m, nil
}
