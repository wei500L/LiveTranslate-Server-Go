// Package metrics holds the process-internal counters exported on the
// gated /metrics endpoint. Values are aggregate counts only — never user
// emails, tokens, URLs or classroom content. The registry is process-wide
// and safe for concurrent use.
package metrics

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
)

// Registry is a flat set of named atomic counters.
type Registry struct {
	mu       sync.Mutex // guards creation only
	counters map[string]*counter
}

type counter struct {
	value atomic.Int64
	help  string
}

var defaultRegistry = &Registry{counters: map[string]*counter{}}

// Default returns the process-wide registry.
func Default() *Registry { return defaultRegistry }

// Counter names (fixed vocabulary; every name is wired — the admin
// dashboard reads the same values through Snapshot). These are PROCESS
// counters: they reset on restart and describe the CURRENT instance since
// boot — never long-term business statistics (those are DB aggregates).
const (
	HTTPRequestsTotal  = "http_requests_total"            // countingMiddleware
	HTTP5xxTotal       = "http_5xx_total"                 // countingMiddleware
	MailSentTotal      = "mail_sent_total"                // MeteredSender
	MailFailedTotal    = "mail_failed_total"              // MeteredSender
	SyncPushBatches    = "sync_push_batches_total"        // syncapi push
	SyncPushConflicts  = "sync_push_conflicts_total"      // syncapi push results
	SyncPullRequests   = "sync_pull_requests_total"       // syncapi pull
	TombstoneGcRuns    = "maintenance_gc_runs_total"      // cleanup loop
	MaintenanceDeleted = "maintenance_rows_deleted_total" // cleanup loop
	LoginEventsPruned  = "login_events_pruned_total"      // cleanup loop
)

// Inc atomically adds one to the named counter, creating it on first use.
func Inc(name string) { Add(name, 1) }

// Add atomically adds delta to the named counter.
func Add(name string, delta int64) {
	c := defaultRegistry.get(name, "")
	c.value.Add(delta)
}

// IncWithHelp records a counter that also carries documentation (used at
// declaration sites so the /metrics output includes HELP lines).
func IncWithHelp(name, help string) {
	c := defaultRegistry.get(name, help)
	c.value.Add(1)
}

func (r *Registry) get(name, help string) *counter {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		c = &counter{help: help}
		r.counters[name] = c
	}
	if help != "" && c.help == "" {
		c.help = help
	}
	return c
}

// Snapshot returns counter name → current value (a stable copy).
func (r *Registry) Snapshot() map[string]int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int64, len(r.counters))
	for name, c := range r.counters {
		out[name] = c.value.Load()
	}
	return out
}

// Render emits the Prometheus text exposition format.
func (r *Registry) Render() string {
	snap := r.Snapshot()
	names := make([]string, 0, len(snap))
	for name := range snap {
		names = append(names, name)
	}
	sort.Strings(names)
	out := ""
	for _, name := range names {
		help := ""
		r.mu.Lock()
		if c, ok := r.counters[name]; ok {
			help = c.help
		}
		r.mu.Unlock()
		if help != "" {
			out += fmt.Sprintf("# HELP %s %s\n", name, help)
		}
		out += fmt.Sprintf("# TYPE %s counter\n%s %d\n", name, name, snap[name])
	}
	return out
}
