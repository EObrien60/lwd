// Package scheduler implements a pure, deterministic placement engine: given
// a set of candidate nodes with live capacity, a target pool, and resource
// requirements, it picks the best node for a surface. It performs no I/O, no
// Docker calls, and spawns no goroutines — it is data-in, data-out so it can
// be unit tested without any live infrastructure. The reconciler is
// responsible for gathering candidates (via node capacity probes) and
// calling Place.
package scheduler

import (
	"fmt"
	"sort"

	"lwd/internal/node"
)

// NodeInfo is a candidate node's placement-relevant state.
type NodeInfo struct {
	Name      string
	Pool      string
	Reachable bool
	Cap       node.Capacity
}

// Requirements describes the resources a surface needs. A zero value means
// "no requirement" for that dimension.
type Requirements struct {
	CPUCores float64
	MemBytes int64
}

// Place picks the best node in pool for req from candidates. It returns an
// error if no reachable node exists in the pool, or if none of the reachable
// nodes in the pool have enough capacity to satisfy req.
func Place(candidates []NodeInfo, pool string, req Requirements) (string, error) {
	if pool == "" {
		pool = "default"
	}

	var inPool []NodeInfo
	for _, c := range candidates {
		if c.Reachable && c.Pool == pool {
			inPool = append(inPool, c)
		}
	}
	if len(inPool) == 0 {
		return "", fmt.Errorf("no reachable nodes in pool %q", pool)
	}

	var fitting []NodeInfo
	for _, c := range inPool {
		if fits(c, req) {
			fitting = append(fitting, c)
		}
	}
	if len(fitting) == 0 {
		return "", fmt.Errorf("no node in pool %q has capacity (need cpu=%.2g memory=%d bytes)", pool, req.CPUCores, req.MemBytes)
	}

	sort.SliceStable(fitting, func(i, j int) bool {
		a, b := fitting[i], fitting[j]
		am, bm := freeMem(a), freeMem(b)
		if am != bm {
			return am > bm
		}
		ac, bc := freeCPU(a), freeCPU(b)
		if ac != bc {
			return ac > bc
		}
		return a.Name < b.Name
	})

	return fitting[0].Name, nil
}

// fits reports whether c has enough capacity to satisfy req. A node whose
// capacity could not be measured live (Cap.Known == false) is optimistically
// assumed to fit any requirement.
func fits(c NodeInfo, req Requirements) bool {
	if !c.Cap.Known {
		return true
	}
	if req.MemBytes != 0 && c.Cap.MemAvailable < req.MemBytes {
		return false
	}
	if req.CPUCores != 0 && float64(c.Cap.CPUCores)-c.Cap.CPUUsed < req.CPUCores {
		return false
	}
	return true
}

// freeMem returns the ranking value for available memory: the live
// MemAvailable when known, else the total memory (0 if that too is unset).
func freeMem(c NodeInfo) int64 {
	if c.Cap.Known {
		return c.Cap.MemAvailable
	}
	return c.Cap.MemTotal
}

// freeCPU returns the ranking value for available CPU: live cores-minus-used
// when known, else the raw core count.
func freeCPU(c NodeInfo) float64 {
	if c.Cap.Known {
		return float64(c.Cap.CPUCores) - c.Cap.CPUUsed
	}
	return float64(c.Cap.CPUCores)
}
