package diagnostics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// RoutingAuditEvent contains non-sensitive metadata for a single generation routing attempt.
type RoutingAuditEvent struct {
	Timestamp      time.Time `json:"timestamp"`
	RequestID      string    `json:"request_id,omitempty"`
	Strategy       string    `json:"strategy"`
	Account        string    `json:"account"`
	AccountID      string    `json:"account_id,omitempty"`
	Classification string    `json:"classification"`

	FiveHourKnown      bool       `json:"five_hour_known,omitempty"`
	FiveHourRemaining  *float64   `json:"five_hour_remaining"`
	FiveHourResetAt    *time.Time `json:"five_hour_reset_at,omitempty"`
	FiveHourResetRatio *float64   `json:"five_hour_reset_ratio,omitempty"`
	FiveHourPace       *float64   `json:"five_hour_pace,omitempty"`

	WeeklyKnown      bool       `json:"weekly_known,omitempty"`
	WeeklyRemaining  *float64   `json:"weekly_remaining"`
	WeeklyResetAt    *time.Time `json:"weekly_reset_at,omitempty"`
	WeeklyResetRatio *float64   `json:"weekly_reset_ratio,omitempty"`
	WeeklyPace       *float64   `json:"weekly_pace,omitempty"`

	KnownWindowCount int      `json:"known_window_count"`
	FreshnessRank    *int     `json:"freshness_rank,omitempty"`
	WorstPace        *float64 `json:"worst_pace,omitempty"`
	TotalPace        *float64 `json:"total_pace,omitempty"`
	RawFloor         *float64 `json:"raw_floor,omitempty"`

	HealthyCount      int      `json:"healthy_count"`
	ReserveCount      int      `json:"reserve_count"`
	HealthyCandidates []string `json:"healthy_candidates,omitempty"`
	ReserveCandidates []string `json:"reserve_candidates,omitempty"`

	Attempt        int    `json:"attempt"`
	Status         int    `json:"status"`
	FailoverReason string `json:"failover_reason,omitempty"`
	Verdict        string `json:"verdict"`
	VerdictDetails string `json:"verdict_details,omitempty"`
}

// AuditOptions configures the routing audit execution.
type AuditOptions struct {
	Duration       time.Duration
	JSONMode       bool
	ReportPath     string
	LogPath        string
	FromStart      bool
	Reader         io.Reader                 // Optional direct reader for testing
	EventChan      <-chan RoutingAuditEvent  // Optional direct event stream for testing
	ProxyEventChan <-chan proxy.RoutingEvent // Direct stream of frozen proxy routing events
}

// AuditSummary contains aggregated metrics for an audit session.
type AuditSummary struct {
	GenerationAttempts int64            `json:"generation_attempts"`
	AccountCounts      map[string]int64 `json:"account_counts"`
	Violations         int64            `json:"violations"`
	Warnings           int64            `json:"warnings"`
	DroppedEvents      int64            `json:"dropped_events"`
	FinalVerdict       string           `json:"final_verdict"`
	Strategy           string           `json:"strategy"`
	Duration           time.Duration    `json:"duration"`
}

// Auditor coordinates routing observation, event evaluation, and report aggregation.
type Auditor struct {
	mu            sync.Mutex
	options       AuditOptions
	summary       AuditSummary
	events        []RoutingAuditEvent
	droppedEvents int64
	startTime     time.Time
}

// NewAuditor creates a new Auditor instance.
func NewAuditor(opts AuditOptions) *Auditor {
	return &Auditor{
		options: opts,
		summary: AuditSummary{
			AccountCounts: make(map[string]int64),
			FinalVerdict:  string(VerdictPass),
		},
		startTime: time.Now(),
	}
}

// Run executes the audit until duration expires, ctx is cancelled, or input is exhausted.
func (a *Auditor) Run(ctx context.Context, stdout, stderr io.Writer) error {
	if a.options.Duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.options.Duration)
		defer cancel()
	}

	pool, _ := storage.LoadPool()
	strategy := config.StrategyMaxQuota
	if pool != nil && pool.Strategy != "" {
		strategy = pool.Strategy
	}
	a.summary.Strategy = strategy

	if !a.options.JSONMode {
		durStr := "manual (Ctrl-C)"
		if a.options.Duration > 0 {
			durStr = a.options.Duration.String()
		}
		stratNote := strategy
		if strategy != config.StrategyMaxQuota {
			stratNote = fmt.Sprintf("%s (reserve headroom checks N/A)", strategy)
		}

		fmt.Fprintln(stdout, "\n  agy-pool routing audit")
		fmt.Fprintf(stdout, "  Strategy : %s\n", stratNote)
		fmt.Fprintf(stdout, "  Duration : %s\n", durStr)
	}

	// 0. In-process proxy event channel mode (direct subscription with frozen RoutingEvent)
	if a.options.ProxyEventChan != nil {
		for {
			select {
			case <-ctx.Done():
				a.summary.Duration = time.Since(a.startTime)
				a.summary.DroppedEvents = proxy.DroppedAuditEvents()
				a.PrintSummary(stdout)
				_ = a.WriteReport()
				return nil
			case ev, ok := <-a.options.ProxyEventChan:
				if !ok {
					a.summary.Duration = time.Since(a.startTime)
					a.summary.DroppedEvents = proxy.DroppedAuditEvents()
					a.PrintSummary(stdout)
					_ = a.WriteReport()
					return nil
				}
				auditEv := EvaluateFrozenRoutingEvent(ev)
				a.RecordEvent(auditEv, stdout)
			}
		}
	}

	// 1. In-process event channel mode (testing or embedded)
	if a.options.EventChan != nil {
		for {
			select {
			case <-ctx.Done():
				a.summary.Duration = time.Since(a.startTime)
				a.PrintSummary(stdout)
				_ = a.WriteReport()
				return nil
			case ev, ok := <-a.options.EventChan:
				if !ok {
					a.summary.Duration = time.Since(a.startTime)
					a.PrintSummary(stdout)
					_ = a.WriteReport()
					return nil
				}
				a.RecordEvent(ev, stdout)
			}
		}
	}

	// 2. Direct reader mode (unit testing with log text)
	if a.options.Reader != nil {
		scanner := bufio.NewScanner(a.options.Reader)
		currentAttempt := 1
		lastFailoverReason := ""

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				a.summary.Duration = time.Since(a.startTime)
				a.PrintSummary(stdout)
				_ = a.WriteReport()
				return nil
			default:
			}

			line := scanner.Text()
			entry, ok := ParseLogLine(line)
			if !ok {
				continue
			}

			if entry.Type == "FAILOVER" {
				lastFailoverReason = entry.FailoverReason
				continue
			}

			if !entry.IsGeneration {
				continue
			}

			var ev RoutingAuditEvent
			if entry.HasAuditMeta {
				ev = EvaluateFrozenRoutingEvent(proxy.RoutingEvent{
					Timestamp:             entry.Timestamp,
					Strategy:              entry.Strategy,
					Account:               entry.AccountName,
					AccountName:           entry.AccountName,
					Classification:        entry.Classification,
					HealthyCandidateCount: entry.HealthyCount,
					ReserveCandidateCount: entry.ReserveCount,
					Attempt:               currentAttempt,
					Status:                entry.Status,
					FailoverReason:        lastFailoverReason,
					Path:                  entry.Path,
				})
			} else {
				currentPool, _ := storage.LoadPool()
				if currentPool == nil {
					currentPool = pool
				}
				ev = EvaluateRoutingAttempt(
					currentPool,
					entry.AccountName,
					entry.Status,
					currentAttempt,
					lastFailoverReason,
					entry.Timestamp,
					"",
				)
			}
			a.RecordEvent(ev, stdout)

			if entry.Status >= 400 && lastFailoverReason != "" {
				currentAttempt++
			} else {
				currentAttempt = 1
				lastFailoverReason = ""
			}
		}

		a.summary.Duration = time.Since(a.startTime)
		a.PrintSummary(stdout)
		_ = a.WriteReport()
		return nil
	}

	// 3. Live log follower mode (auditing live daemon from runtime log file)
	return a.runLogFollower(ctx, stdout, stderr, pool)
}

// RunAuditRouting executes the audit-routing command with given options.
func RunAuditRouting(ctx context.Context, opts AuditOptions, stdout, stderr io.Writer) int {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
	}

	auditor := NewAuditor(opts)
	err := auditor.Run(ctx, stdout, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "audit-routing error: %v\n", err)
		return 1
	}

	if auditor.summary.Violations > 0 {
		return 1
	}
	return 0
}
