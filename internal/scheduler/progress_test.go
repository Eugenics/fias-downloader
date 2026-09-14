package scheduler

import (
	"testing"
	"time"
)

func TestProgressPersistenceThrottlesIntermediateUpdatesAndKeepsFinal(t *testing.T) {
	now := time.Unix(100, 0)
	p := newProgressPersistence(30*time.Second, func() time.Time { return now })

	if !p.shouldPersist(0, 100) {
		t.Fatal("initial progress must be persisted")
	}
	now = now.Add(time.Second)
	if p.shouldPersist(10, 100) {
		t.Fatal("intermediate progress before interval must be throttled")
	}
	now = now.Add(29 * time.Second)
	if !p.shouldPersist(20, 100) {
		t.Fatal("progress at persistence interval must be persisted")
	}
	now = now.Add(time.Second)
	if !p.shouldPersist(100, 100) {
		t.Fatal("final progress must be persisted regardless of interval")
	}
}
