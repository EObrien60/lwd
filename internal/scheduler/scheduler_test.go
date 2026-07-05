package scheduler

import (
	"strings"
	"testing"

	"lwd/internal/node"
)

func TestPlacePicksMostFreeMemory(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 1, MemAvailable: 1000}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 1, MemAvailable: 3000}},
		{Name: "c", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 1, MemAvailable: 2000}},
	}
	got, err := Place(candidates, "default", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("expected b (most free mem), got %q", got)
	}
}

func TestPlacePoolFilter(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "other", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}},
	}
	got, err := Place(candidates, "default", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("expected b (only default-pool candidate), got %q", got)
	}
}

func TestPlaceExcludesUnreachable(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: false, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}},
	}
	got, err := Place(candidates, "default", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("expected b (a is unreachable), got %q", got)
	}
}

func TestPlaceRequirementsFilterMem(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 500}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 2000}},
	}
	got, err := Place(candidates, "default", Requirements{MemBytes: 1000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("expected b (a lacks memory), got %q", got)
	}
}

func TestPlaceRequirementsFilterCpu(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 2, CPUUsed: 1.5, MemAvailable: 5000}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 0, MemAvailable: 1000}},
	}
	got, err := Place(candidates, "default", Requirements{CPUCores: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("expected b (a lacks free cpu: 2-1.5=0.5 < 2), got %q", got)
	}
}

func TestPlaceUnknownTreatedAsFree(t *testing.T) {
	candidates := []NodeInfo{
		// Known:false must fit any requirement, regardless of MemAvailable/CPUUsed.
		{Name: "unknown", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: false, MemTotal: 8000, MemAvailable: 0, CPUCores: 1, CPUUsed: 99}},
		{Name: "known", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 0, MemAvailable: 4000}},
	}
	got, err := Place(candidates, "default", Requirements{CPUCores: 8, MemBytes: 100000})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "unknown" {
		t.Fatalf("expected unknown (Known:false always fits), got %q", got)
	}

	// Ranking: unknown node ranks by MemTotal (8000) vs known's MemAvailable (4000) -> unknown wins.
	candidates2 := []NodeInfo{
		{Name: "unknown", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: false, MemTotal: 8000}},
		{Name: "known", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 0, MemAvailable: 4000}},
	}
	got2, err := Place(candidates2, "default", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got2 != "unknown" {
		t.Fatalf("expected unknown to rank by MemTotal=8000 and win, got %q", got2)
	}
}

func TestPlaceTieBreakByName(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "zeta", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 1, MemAvailable: 1000}},
		{Name: "alpha", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 1, MemAvailable: 1000}},
		{Name: "mid", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, CPUUsed: 1, MemAvailable: 1000}},
	}
	got, err := Place(candidates, "default", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "alpha" {
		t.Fatalf("expected alpha (lexical tie-break), got %q", got)
	}
}

func TestPlaceNoReachableNodes(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: false, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}},
	}
	_, err := Place(candidates, "default", Requirements{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "no schedulable node in pool") {
		t.Fatalf("expected 'no schedulable node in pool' error, got: %v", err)
	}
}

func TestPlaceNoneFit(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 2, CPUUsed: 2, MemAvailable: 100}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 1, CPUUsed: 0, MemAvailable: 100}},
	}
	_, err := Place(candidates, "default", Requirements{CPUCores: 4, MemBytes: 100000})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "has capacity") {
		t.Fatalf("expected 'has capacity' error, got: %v", err)
	}
}

// TestPlaceExcludesCordoned covers Phase 11b Task 2: a cordoned node
// (Schedulable: false) must never receive a new placement, even when it is
// otherwise the most-free candidate in the pool. If it is the only candidate
// in the pool, Place must report an error (the same "no schedulable node in
// pool" message used for unreachable nodes — a cordoned node is simply not a
// candidate) rather than picking it.
func TestPlaceExcludesCordoned(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: false, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}},
		{Name: "b", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}},
	}
	got, err := Place(candidates, "default", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "b" {
		t.Fatalf("expected b (a is cordoned), got %q", got)
	}

	onlyCordoned := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: false, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 9000}},
	}
	_, err = Place(onlyCordoned, "default", Requirements{})
	if err == nil {
		t.Fatal("expected error when the only candidate is cordoned, got nil")
	}
	// A cordoned node is simply not a candidate, same as an unreachable one:
	// both share the single "no schedulable node in pool %q" empty-candidate
	// error (a node that is unreachable OR cordoned is "not schedulable").
	if !strings.Contains(err.Error(), "no schedulable node in pool") {
		t.Fatalf("expected 'no schedulable node in pool' error, got: %v", err)
	}
}

func TestPlaceDefaultPool(t *testing.T) {
	candidates := []NodeInfo{
		{Name: "a", Pool: "default", Reachable: true, Schedulable: true, Cap: node.Capacity{Known: true, CPUCores: 4, MemAvailable: 1000}},
	}
	got, err := Place(candidates, "", Requirements{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "a" {
		t.Fatalf("expected a (pool=='' treated as default), got %q", got)
	}
}
