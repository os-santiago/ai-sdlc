// Package memlimit sets the Go runtime's soft memory limit below the
// container's memory limit. A process that holds a large workspace tree
// (up to the workspace bundle bounds) then collects garbage before the
// kernel's out-of-memory killer would act, instead of letting the heap grow
// to twice its live size. It grants nothing and changes no other setting.
package memlimit

import (
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Fraction of the container limit used as the soft limit: the rest covers
// memory the Go runtime does not count (thread stacks outside the heap,
// mapped files, other processes in the container).
const Fraction = 0.85

var limitFiles = []string{
	"/sys/fs/cgroup/memory.max",                   // cgroup v2
	"/sys/fs/cgroup/memory/memory.limit_in_bytes", // cgroup v1
}

// Apply sets the soft limit from the container's cgroup and returns it, or
// returns 0 and changes nothing when GOMEMLIMIT is set or no finite limit
// is readable.
func Apply() int64 {
	if os.Getenv("GOMEMLIMIT") != "" {
		return 0
	}
	limit := fromCgroup(os.ReadFile)
	if limit > 0 {
		debug.SetMemoryLimit(limit)
	}
	return limit
}

func fromCgroup(read func(string) ([]byte, error)) int64 {
	for _, name := range limitFiles {
		raw, err := read(name)
		if err != nil || len(raw) > 64 {
			continue
		}
		value := strings.TrimSpace(string(raw))
		bytes, err := strconv.ParseInt(value, 10, 64)
		// "max" (v2) and the v1 sentinel near 2^63 mean no limit.
		if err != nil || bytes <= 0 || bytes >= 1<<50 {
			return 0
		}
		return int64(float64(bytes) * Fraction)
	}
	return 0
}
