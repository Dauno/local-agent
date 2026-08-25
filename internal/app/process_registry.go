package app

import (
	"strconv"
	"sync"

	"github.com/Dauno/slack-local-agent/internal/port"
)

var _ port.ExternalAgentProcessRegistry = (*inProcessRegistry)(nil)

// inProcessRegistry tracks the best-effort liveness of the ACP process
// spawned for each active job attempt. It is intentionally in-memory only:
// after restart the registry is empty and liveness reports unknown (nil),
// never dead.
type inProcessRegistry struct {
	mu    sync.Mutex
	procs map[string]int
}

func newInProcessRegistry() *inProcessRegistry {
	return &inProcessRegistry{procs: make(map[string]int)}
}

func (r *inProcessRegistry) Register(jobID string, attempt int, pid int) {
	if r == nil || jobID == "" || pid <= 0 {
		return
	}
	r.mu.Lock()
	if len(r.procs) >= maxRegisteredProcesses {
		for key, existing := range r.procs {
			if !processExists(existing) {
				delete(r.procs, key)
			}
		}
	}
	r.procs[registryKey(jobID, attempt)] = pid
	r.mu.Unlock()
}

// maxRegisteredProcesses bounds the in-memory liveness registry; dead entries
// are swept during registration so memory stays constant with job churn.
const maxRegisteredProcesses = 256

func (r *inProcessRegistry) ProcessAlive(jobID string, attempt int) *bool {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	pid, exists := r.procs[registryKey(jobID, attempt)]
	r.mu.Unlock()
	if !exists {
		return nil
	}
	alive := processExists(pid)
	return &alive
}

func registryKey(jobID string, attempt int) string {
	return jobID + "\x00" + strconv.Itoa(attempt)
}
