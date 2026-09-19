package observability

import (
	"sync"
	"testing"
)

func TestMetrics_AtomicOperationsAndReset(t *testing.T) {
	Reset()

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RecordGenerationRequest()
			RecordGenerationSuccess()
			RecordRoutingDecision()
			RecordFailoverAttempt()
			RecordFailoverSuccess()
			RecordRestrictedSkip()
			RecordQuotaRefreshAttempt()
			RecordQuotaRefreshSuccess()
			RecordQuotaRefreshFailure()
			RecordAuthRefreshAttempt()
			RecordAuthRefreshSuccess()
			RecordAuthRefreshFailure()
			RecordPersistenceError()
			RecordNoReplayPrevented()
		}()
	}
	wg.Wait()

	snap := GetSnapshot()
	if snap.GenerationRequests != 100 {
		t.Errorf("expected 100 generation requests, got %d", snap.GenerationRequests)
	}
	if snap.GenerationSuccess != 100 {
		t.Errorf("expected 100 generation success, got %d", snap.GenerationSuccess)
	}
	if snap.RoutingDecisions != 100 {
		t.Errorf("expected 100 routing decisions, got %d", snap.RoutingDecisions)
	}
	if snap.FailoverAttempts != 100 {
		t.Errorf("expected 100 failover attempts, got %d", snap.FailoverAttempts)
	}
	if snap.FailoverSuccess != 100 {
		t.Errorf("expected 100 failover success, got %d", snap.FailoverSuccess)
	}
	if snap.RestrictedSkips != 100 {
		t.Errorf("expected 100 restricted skips, got %d", snap.RestrictedSkips)
	}
	if snap.QuotaRefreshAttempts != 100 {
		t.Errorf("expected 100 quota refresh attempts, got %d", snap.QuotaRefreshAttempts)
	}
	if snap.QuotaRefreshSuccess != 100 {
		t.Errorf("expected 100 quota refresh success, got %d", snap.QuotaRefreshSuccess)
	}
	if snap.QuotaRefreshFailure != 100 {
		t.Errorf("expected 100 quota refresh failure, got %d", snap.QuotaRefreshFailure)
	}
	if snap.AuthRefreshAttempts != 100 {
		t.Errorf("expected 100 auth refresh attempts, got %d", snap.AuthRefreshAttempts)
	}
	if snap.AuthRefreshSuccess != 100 {
		t.Errorf("expected 100 auth refresh success, got %d", snap.AuthRefreshSuccess)
	}
	if snap.AuthRefreshFailure != 100 {
		t.Errorf("expected 100 auth refresh failure, got %d", snap.AuthRefreshFailure)
	}
	if snap.PersistenceErrors != 100 {
		t.Errorf("expected 100 persistence errors, got %d", snap.PersistenceErrors)
	}
	if snap.NoReplayPrevented != 100 {
		t.Errorf("expected 100 no replay prevented, got %d", snap.NoReplayPrevented)
	}

	Reset()
	snap = GetSnapshot()
	if snap.GenerationRequests != 0 || snap.GenerationSuccess != 0 || snap.RoutingDecisions != 0 {
		t.Errorf("expected all counters reset to zero, got %+v", snap)
	}
}

func TestFormatNumber(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{
		{0, "0"},
		{9, "9"},
		{99, "99"},
		{999, "999"},
		{1000, "1,000"},
		{12430, "12,430"},
		{12401, "12,401"},
		{12988, "12,988"},
		{1000000, "1,000,000"},
		{18446744073709551615, "18,446,744,073,709,551,615"},
	}

	for _, c := range cases {
		got := FormatNumber(c.in)
		if got != c.want {
			t.Errorf("FormatNumber(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestInFlightTracking_ConcurrentLifecycle(t *testing.T) {
	Reset()

	if inFlight := GetInFlightGeneration("acc1"); inFlight != 0 {
		t.Fatalf("expected initial in-flight to be 0, got %d", inFlight)
	}
	if all := GetAllInFlightGenerations(); all != nil {
		t.Fatalf("expected initial GetAllInFlightGenerations to be nil, got %+v", all)
	}

	done1 := StartInFlightGeneration("acc1")
	if inFlight := GetInFlightGeneration("acc1"); inFlight != 1 {
		t.Fatalf("expected in-flight 1, got %d", inFlight)
	}

	done2 := StartInFlightGeneration("acc1")
	if inFlight := GetInFlightGeneration("acc1"); inFlight != 2 {
		t.Fatalf("expected in-flight 2, got %d", inFlight)
	}

	all := GetAllInFlightGenerations()
	if all == nil || all["acc1"] != 2 {
		t.Fatalf("expected GetAllInFlightGenerations to have acc1=2, got %+v", all)
	}

	// Decrement once
	done1()
	if inFlight := GetInFlightGeneration("acc1"); inFlight != 1 {
		t.Fatalf("expected in-flight 1 after done1, got %d", inFlight)
	}

	// Idempotent: done1 called again should not decrement further
	done1()
	if inFlight := GetInFlightGeneration("acc1"); inFlight != 1 {
		t.Fatalf("expected in-flight still 1 after duplicate done1, got %d", inFlight)
	}

	// Decrement second
	done2()
	if inFlight := GetInFlightGeneration("acc1"); inFlight != 0 {
		t.Fatalf("expected in-flight 0 after done2, got %d", inFlight)
	}
	if all := GetAllInFlightGenerations(); all != nil {
		t.Fatalf("expected GetAllInFlightGenerations to be nil after all done, got %+v", all)
	}

	// Concurrency test: 50 goroutines
	var wg sync.WaitGroup
	var changeCalls int
	var changeMu sync.Mutex
	SetOnInFlightChange(func() {
		changeMu.Lock()
		changeCalls++
		changeMu.Unlock()
	})
	defer SetOnInFlightChange(nil)

	const count = 50
	dones := make([]func(), count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dones[idx] = StartInFlightGeneration("acc_concurrent")
		}(i)
	}
	wg.Wait()

	if inFlight := GetInFlightGeneration("acc_concurrent"); inFlight != count {
		t.Fatalf("expected in-flight %d, got %d", count, inFlight)
	}

	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			dones[idx]()
		}(i)
	}
	wg.Wait()

	if inFlight := GetInFlightGeneration("acc_concurrent"); inFlight != 0 {
		t.Fatalf("expected in-flight to return to 0, got %d", inFlight)
	}

	changeMu.Lock()
	if changeCalls < count*2 {
		t.Errorf("expected at least %d change callback calls, got %d", count*2, changeCalls)
	}
	changeMu.Unlock()
}
