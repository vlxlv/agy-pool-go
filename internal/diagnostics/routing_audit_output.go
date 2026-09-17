package diagnostics

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func formatResetCountdown(t *time.Time, now time.Time) string {
	if t == nil {
		return ""
	}
	d := t.Sub(now)
	if d <= 0 {
		return "reset now"
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	mins := int(d.Minutes()) % 60
	if days > 0 {
		return fmt.Sprintf("reset in %dd%dh", days, hours)
	}
	if hours > 0 {
		return fmt.Sprintf("reset in %dh%dm", hours, mins)
	}
	if mins > 0 {
		return fmt.Sprintf("reset in %dm", mins)
	}
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("reset in %ds", secs)
}

// RecordEvent records a verified routing event into summaries and logs.
func (a *Auditor) RecordEvent(event RoutingAuditEvent, out io.Writer) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.summary.GenerationAttempts++
	a.summary.AccountCounts[event.Account]++

	switch strings.ToUpper(event.Verdict) {
	case string(VerdictViolation):
		a.summary.Violations++
		a.summary.FinalVerdict = string(VerdictViolation)
	case string(VerdictWarning):
		a.summary.Warnings++
		if a.summary.FinalVerdict != string(VerdictViolation) {
			a.summary.FinalVerdict = string(VerdictWarning)
		}
	}

	if a.summary.Strategy == "" {
		a.summary.Strategy = event.Strategy
	}

	if a.options.ReportPath != "" {
		a.events = append(a.events, event)
	}

	// Format output to out
	if out != nil {
		if a.options.JSONMode {
			b, _ := json.Marshal(event)
			fmt.Fprintln(out, string(b))
		} else {
			attemptStr := ""
			if event.Attempt > 1 {
				attemptStr = fmt.Sprintf(" (Attempt %d)", event.Attempt)
			}
			tsStr := event.Timestamp.Format("15:04:05")

			fmt.Fprintf(out, "\n  [%s] %s%s\n", tsStr, event.Account, attemptStr)
			tierDisp := strings.Title(event.Classification)
			if tierDisp == "" {
				tierDisp = "Unknown"
			}
			fmt.Fprintf(out, "    Tier    : %s\n", tierDisp)

			if event.FiveHourRemaining != nil {
				resetInfo := ""
				if event.FiveHourResetAt != nil {
					resetInfo = fmt.Sprintf(" (%s)", formatResetCountdown(event.FiveHourResetAt, event.Timestamp))
				}
				fmt.Fprintf(out, "    5H      : %.1f%%%s\n", *event.FiveHourRemaining*100.0, resetInfo)
			} else {
				fmt.Fprintf(out, "    5H      : N/A\n")
			}

			if event.WeeklyRemaining != nil {
				resetInfo := ""
				if event.WeeklyResetAt != nil {
					resetInfo = fmt.Sprintf(" (%s)", formatResetCountdown(event.WeeklyResetAt, event.Timestamp))
				}
				fmt.Fprintf(out, "    Weekly  : %.1f%%%s\n", *event.WeeklyRemaining*100.0, resetInfo)
			} else {
				fmt.Fprintf(out, "    Weekly  : N/A\n")
			}

			if event.WorstPace != nil {
				fmt.Fprintf(out, "    WorstPace: %+.4f\n", *event.WorstPace)
			}

			fmt.Fprintf(out, "    Status  : %d\n", event.Status)

			verdictUpper := strings.ToUpper(event.Verdict)
			if verdictUpper == string(VerdictViolation) && event.VerdictDetails != "" {
				fmt.Fprintf(out, "    Verdict : %s (%s)\n", verdictUpper, event.VerdictDetails)
			} else if verdictUpper == string(VerdictWarning) && event.VerdictDetails != "" {
				fmt.Fprintf(out, "    Verdict : %s (%s)\n", verdictUpper, event.VerdictDetails)
			} else {
				fmt.Fprintf(out, "    Verdict : %s\n", verdictUpper)
			}

			if event.FailoverReason != "" {
				fmt.Fprintf(out, "    Failover: %s\n", event.FailoverReason)
			}
		}
	}
}

// PrintSummary prints the final summary to out.
func (a *Auditor) PrintSummary(out io.Writer) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.options.JSONMode {
		return
	}

	fmt.Fprintln(out, "\n  Summary")
	fmt.Fprintln(out, "  -------")
	fmt.Fprintf(out, "  Generation attempts : %d\n", a.summary.GenerationAttempts)

	// List known accounts from pool if available, plus any observed accounts
	accNames := make([]string, 0, len(a.summary.AccountCounts))
	for name := range a.summary.AccountCounts {
		accNames = append(accNames, name)
	}
	sort.Strings(accNames)

	for _, name := range accNames {
		fmt.Fprintf(out, "  %-20s: %d\n", name, a.summary.AccountCounts[name])
	}

	fmt.Fprintf(out, "  Violations          : %d\n", a.summary.Violations)
	fmt.Fprintf(out, "  Warnings            : %d\n", a.summary.Warnings)
	if a.droppedEvents > 0 {
		fmt.Fprintf(out, "  Dropped events      : %d\n", a.droppedEvents)
	}

	fmt.Fprintf(out, "\n  Final verdict: %s\n\n", a.summary.FinalVerdict)
}

// WriteReport writes audit results to report file if configured.
func (a *Auditor) WriteReport() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.options.ReportPath == "" {
		return nil
	}

	dir := filepath.Dir(a.options.ReportPath)
	if dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0755)
	}

	f, err := os.Create(a.options.ReportPath)
	if err != nil {
		return err
	}
	defer f.Close()

	if a.options.JSONMode || strings.HasSuffix(a.options.ReportPath, ".json") {
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		reportData := map[string]any{
			"summary": a.summary,
			"events":  a.events,
		}
		return enc.Encode(reportData)
	}

	// Human readable report
	fmt.Fprintf(f, "agy-pool routing audit report\n")
	fmt.Fprintf(f, "Strategy : %s\n", a.summary.Strategy)
	fmt.Fprintf(f, "Duration : %s\n\n", a.summary.Duration)
	for _, ev := range a.events {
		b, _ := json.Marshal(ev)
		fmt.Fprintln(f, string(b))
	}
	fmt.Fprintln(f, "\nSummary")
	fmt.Fprintln(f, "-------")
	fmt.Fprintf(f, "Generation attempts : %d\n", a.summary.GenerationAttempts)
	for name, count := range a.summary.AccountCounts {
		fmt.Fprintf(f, "%-20s: %d\n", name, count)
	}
	fmt.Fprintf(f, "Violations          : %d\n", a.summary.Violations)
	fmt.Fprintf(f, "Warnings            : %d\n", a.summary.Warnings)
	fmt.Fprintf(f, "\nFinal verdict: %s\n", a.summary.FinalVerdict)
	return nil
}
