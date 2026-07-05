package node

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Capacity reports a node's total and available resources, as best known.
// Known is false when the values could not be measured live (e.g. a
// non-Linux dev host with no /proc, or a remote docker-over-ssh node where
// only totals — not live usage — are available via the Docker API).
type Capacity struct {
	CPUCores     int     `json:"cpu_cores"`
	CPUUsed      float64 `json:"cpu_used"`
	MemTotal     int64   `json:"mem_total"`
	MemAvailable int64   `json:"mem_available"`
	DiskTotal    int64   `json:"disk_total"`
	DiskFree     int64   `json:"disk_free"`
	Known        bool    `json:"known"`
}

// parseMeminfo parses the contents of /proc/meminfo (or an equivalent
// reader) and returns MemTotal and MemAvailable in bytes. Values in
// /proc/meminfo are reported in kB; this converts to bytes (×1024). Returns
// an error if MemTotal is not found.
func parseMeminfo(r io.Reader) (total, avail int64, err error) {
	sc := bufio.NewScanner(r)
	foundTotal := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			v, perr := parseMeminfoValue(line)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse MemTotal: %w", perr)
			}
			total = v * 1024
			foundTotal = true
		case strings.HasPrefix(line, "MemAvailable:"):
			v, perr := parseMeminfoValue(line)
			if perr != nil {
				return 0, 0, fmt.Errorf("parse MemAvailable: %w", perr)
			}
			avail = v * 1024
		}
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	if !foundTotal {
		return 0, 0, fmt.Errorf("MemTotal not found")
	}
	return total, avail, nil
}

// parseMeminfoValue extracts the numeric kB value from a /proc/meminfo line
// of the form "Label:    12345 kB".
func parseMeminfoValue(line string) (int64, error) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed line %q", line)
	}
	return strconv.ParseInt(fields[1], 10, 64)
}

// parseLoadavg1 parses the first whitespace-separated field of
// /proc/loadavg (the 1-minute load average) as a float64.
func parseLoadavg1(r io.Reader) (float64, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, fmt.Errorf("empty loadavg")
	}
	return strconv.ParseFloat(fields[0], 64)
}

// readProcCapacity reads live capacity from /proc and syscall.Statfs. It
// returns an error on any failure (e.g. no /proc, as on non-Linux hosts);
// callers are responsible for falling back gracefully.
func readProcCapacity() (Capacity, error) {
	meminfoFile, err := os.Open("/proc/meminfo")
	if err != nil {
		return Capacity{}, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer meminfoFile.Close()
	memTotal, memAvail, err := parseMeminfo(meminfoFile)
	if err != nil {
		return Capacity{}, fmt.Errorf("parse /proc/meminfo: %w", err)
	}

	loadavgFile, err := os.Open("/proc/loadavg")
	if err != nil {
		return Capacity{}, fmt.Errorf("open /proc/loadavg: %w", err)
	}
	defer loadavgFile.Close()
	cpuUsed, err := parseLoadavg1(loadavgFile)
	if err != nil {
		return Capacity{}, fmt.Errorf("parse /proc/loadavg: %w", err)
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil {
		return Capacity{}, fmt.Errorf("statfs /: %w", err)
	}

	return Capacity{
		CPUCores:     runtime.NumCPU(),
		CPUUsed:      cpuUsed,
		MemTotal:     memTotal,
		MemAvailable: memAvail,
		DiskTotal:    int64(st.Blocks) * int64(st.Bsize),
		DiskFree:     int64(st.Bavail) * int64(st.Bsize),
		Known:        true,
	}, nil
}
