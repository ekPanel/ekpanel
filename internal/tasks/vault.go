package tasks

import (
	"sync"
	"time"
)

// vaultTTL bounds how long secrets wait for their task.
const vaultTTL = time.Hour

// vault holds secrets for tasks in memory only. Passwords a task needs (a new WordPress
// admin password, a new database password) are never written to the tasks table: if the
// panel restarts before the task runs, the secrets are gone and the task fails cleanly.
type vault struct {
	mu      sync.Mutex
	entries map[string]vaultEntry
}

type vaultEntry struct {
	secrets map[string]string
	expires time.Time
}

func newVault() *vault { return &vault{entries: map[string]vaultEntry{}} }

func (v *vault) put(taskID string, secrets map[string]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := time.Now()
	for id, e := range v.entries {
		if now.After(e.expires) {
			delete(v.entries, id)
		}
	}
	v.entries[taskID] = vaultEntry{secrets: secrets, expires: now.Add(vaultTTL)}
}

// take returns and forgets a task's secrets.
func (v *vault) take(taskID string) (map[string]string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.entries[taskID]
	delete(v.entries, taskID)
	if !ok || time.Now().After(e.expires) {
		return nil, false
	}
	return e.secrets, true
}
