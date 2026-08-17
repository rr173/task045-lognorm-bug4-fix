package lognorm

import (
	"fmt"
	"testing"
	"time"
)

func TestProbe_ConcurrentIngestIDs(t *testing.T) {
	const workers = 32
	start := make(chan struct{})
	done := make(chan struct{}, workers)
	svc := NewService()
	now := time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)
	for i := 0; i < workers; i++ {
		go func(i int) {
			<-start
			svc.Ingest([]string{fmt.Sprintf("worker-%d", i)}, now)
			done <- struct{}{}
		}(i)
	}
	close(start)
	for i := 0; i < workers; i++ {
		<-done
	}

	recs := svc.Query(Query{Limit: workers + 1})
	if len(recs) != workers {
		t.Fatalf("records=%d want %d", len(recs), workers)
	}
	seen := make(map[string]bool, workers)
	for _, rec := range recs {
		if seen[rec.ID] {
			t.Fatalf("duplicate record id %q", rec.ID)
		}
		seen[rec.ID] = true
	}
}
