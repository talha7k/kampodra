package command

import (
	"sync"
	"sync/atomic"
	"testing"

	migrateadapter "github.com/talha7k/kampodra/internal/adapter/migrate"
)

// The bounded-parallel tenant pool: at most N migrations run concurrently,
// every item runs exactly once, and the success tally is exact.

func TestRunBoundedRespectsTheConcurrencyBound(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 8} {
		items := make([]migrateadapter.Job, 32)
		for i := range items {
			items[i] = migrateadapter.Job{Path: "/data/tenants/tenant_x.db", NS: "tenant_x"}
		}
		var cur, maxConcurrent, runs int64
		var mu sync.Mutex
		ok := runBounded(n, items, func(migrateadapter.Job) bool {
			c := atomic.AddInt64(&cur, 1)
			mu.Lock()
			if c > maxConcurrent {
				maxConcurrent = c
			}
			mu.Unlock()
			atomic.AddInt64(&runs, 1)
			atomic.AddInt64(&cur, -1)
			return true
		})
		if ok != len(items) {
			t.Errorf("n=%d: ok = %d, want %d", n, ok, len(items))
		}
		if runs != int64(len(items)) {
			t.Errorf("n=%d: runs = %d, want %d (every item runs exactly once)", n, runs, len(items))
		}
		if maxConcurrent > int64(n) {
			t.Errorf("n=%d: observed %d concurrent workers — the bound leaked", n, maxConcurrent)
		}
	}
}

func TestRunBoundedCountsFailuresWithoutBlockingOthers(t *testing.T) {
	items := []migrateadapter.Job{
		{Path: "/a", NS: "a"}, {Path: "/b", NS: "b"}, {Path: "/c", NS: "c"},
	}
	ok := runBounded(4, items, func(j migrateadapter.Job) bool {
		return j.NS != "b" // one bad tenant must not block the others
	})
	if ok != 2 {
		t.Errorf("ok = %d, want 2 (the failure is collected, the rest still run)", ok)
	}
}

func TestRunBoundedEmptyIsZero(t *testing.T) {
	if got := runBounded(4, nil, func(migrateadapter.Job) bool { return true }); got != 0 {
		t.Errorf("runBounded(nil) = %d, want 0", got)
	}
}
