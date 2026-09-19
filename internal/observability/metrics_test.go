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
