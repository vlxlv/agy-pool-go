package daemon_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/observability"
)

func TestRuntimeStatsWriter_100EventCoalescenceAndFinalZero(t *testing.T) {
	observability.ResetInFlight()
	defer observability.ResetInFlight()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var writeCount atomic.Int64
	var lastWrittenInFlight atomic.Value

	customWrite := func(path string, stats daemon.RuntimeStats) error {
		writeCount.Add(1)
		var val int64
		if stats.InFlight != nil {
			val = stats.InFlight["acc-stress-1"]
		}
		lastWrittenInFlight.Store(val)
		return nil
	}

	throttle := 150 * time.Millisecond
	writer := daemon.StartRuntimeStatsWriter(ctx, 12345, time.Now(), "/dev/null", throttle, 5*time.Second)
	writer.WriteFunc = customWrite
	observability.SetOnInFlightChange(writer.Trigger)
	defer observability.SetOnInFlightChange(nil)

	// Launch 10 goroutines each running 10 cycles (100 total request start/finish events = 200 transitions)
	const (
		numGoroutines = 10
		cyclesPerGo   = 10
	)
	var wg sync.WaitGroup
	wg.Add(numGoroutines)

	startTime := time.Now()
	for g := 0; g < numGoroutines; g++ {
		go func(gID int) {
			defer wg.Done()
			for i := 0; i < cyclesPerGo; i++ {
				done := observability.StartInFlightGeneration("acc-stress-1")
				time.Sleep(1 * time.Millisecond)
				done()
			}
		}(g)
	}

	wg.Wait()
	burstDuration := time.Since(startTime)

	// Verify that in-memory in-flight is 0 immediately after all cycles complete
	inMem := observability.GetInFlightGeneration("acc-stress-1")
	if inMem != 0 {
		t.Fatalf("expected in-memory in-flight to be 0, got %d", inMem)
	}

	// Allow throttle window + margin to ensure the trailing coalesce timer flushes
	time.Sleep(throttle + 100*time.Millisecond)

	writer.Stop()

	writes := writeCount.Load()
	t.Logf("100 events completed in %v: total writes = %d (coalesced from 200 raw transitions)", burstDuration, writes)

	// Proves coalescence: 200 raw transitions coalesced into very few writes (expected <= 5)
	if writes > 6 {
		t.Errorf("expected writes to be coalesced to <= 6, got %d writes", writes)
	}
	if writes == 0 {
		t.Errorf("expected at least 1 coalesced write, got 0")
	}

	// Proves final written in-flight state is 0
	lastVal, ok := lastWrittenInFlight.Load().(int64)
	if !ok || lastVal != 0 {
		t.Errorf("expected final written in-flight to be 0, got %v (ok=%v)", lastVal, ok)
	}
}

func TestRuntimeStatsWriter_MultipleAccountsStressCoalescence(t *testing.T) {
	observability.ResetInFlight()
	defer observability.ResetInFlight()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var writeCount atomic.Int64
	throttle := 150 * time.Millisecond
	writer := daemon.StartRuntimeStatsWriter(ctx, 99999, time.Now(), "/dev/null", throttle, 5*time.Second)
	writer.WriteFunc = func(path string, stats daemon.RuntimeStats) error {
		writeCount.Add(1)
		return nil
	}
	observability.SetOnInFlightChange(writer.Trigger)
	defer observability.SetOnInFlightChange(nil)

	const numAccs = 5
	const perAcc = 20 // 5 * 20 = 100 requests = 200 transitions
	var wg sync.WaitGroup
	wg.Add(numAccs)

	for a := 0; a < numAccs; a++ {
		accID := fmt.Sprintf("acc-multi-%d", a)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < perAcc; i++ {
				done := observability.StartInFlightGeneration(id)
				time.Sleep(500 * time.Microsecond)
				done()
			}
		}(accID)
	}

	wg.Wait()

	// Verify all accounts reached in-memory 0
	allInFlight := observability.GetAllInFlightGenerations()
	for id, cnt := range allInFlight {
		if cnt != 0 {
			t.Errorf("expected in-flight for %s to be 0, got %d", id, cnt)
		}
	}

	time.Sleep(throttle + 100*time.Millisecond)
	writer.Stop()

	writes := writeCount.Load()
	t.Logf("Multi-account 100 events: total writes = %d", writes)
	if writes > 6 {
		t.Errorf("expected coalesced writes <= 6, got %d", writes)
	}
}
