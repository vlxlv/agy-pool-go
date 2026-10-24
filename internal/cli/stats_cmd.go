package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/diagnostics"
	"github.com/vlxlv/agy-pool-go/internal/quota"
)

// StatsCmd tracks and evaluates the effectiveness of routing and load-balancing algorithms.
func StatsCmd(stdout, stderr io.Writer, args ...string) int {
	ctx, rem, err := resolveContextForCmd(args)
	if err != nil {
		fmt.Fprintf(stderr, "%s[Error] Failed to resolve instance: %v%s\n", clrRed, err, clrReset)
		return 1
	}

	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var jsonOutput bool
	var windowHours float64

	fs.BoolVar(&jsonOutput, "json", false, "Output full diagnostic metrics in structured JSON format for AI analysis")
	fs.Float64Var(&windowHours, "hours", 4.0, "Rolling analysis window in hours (default: 4.0)")
	fs.Float64Var(&windowHours, "H", 4.0, "Rolling analysis window in hours (default: 4.0)")

	if err := fs.Parse(rem); err != nil {
		return 2
	}

	windowDuration := time.Duration(windowHours * float64(time.Hour))
	report, err := diagnostics.ComputeEffectivenessReport(ctx, windowDuration)
	if err != nil {
		if jsonOutput {
			out, _ := json.MarshalIndent(map[string]any{
				"status": "error",
				"error":  err.Error(),
			}, "", "  ")
			fmt.Fprintln(stdout, string(out))
		} else {
			fmt.Fprintf(stderr, "%s[Error] Failed to compute effectiveness report: %v%s\n", clrRed, err, clrReset)
		}
		return 1
	}

	if jsonOutput {
		out, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			fmt.Fprintf(stderr, "%s[Error] JSON encoding error: %v%s\n", clrRed, err, clrReset)
			return 1
		}
		fmt.Fprintln(stdout, string(out))
		return 0
	}

	renderTerminalReport(stdout, report)
	return 0
}

func renderTerminalReport(w io.Writer, r *diagnostics.EffectivenessReport) {
	fmt.Fprintf(w, "\n====================================================================\n")
	fmt.Fprintf(w, "           Algorithm Routing & Effectiveness Report\n")
	fmt.Fprintf(w, "====================================================================\n")

	gwStr := r.Gateway.Status
	if gwStr == "RUNNING" {
		gwStr = clrGreen + "RUNNING" + clrReset + fmt.Sprintf(" (PID %d, Uptime: %s)", r.Gateway.PID, r.Gateway.Uptime)
	} else {
		gwStr = clrYellow + "STOPPED" + clrReset
	}

	fmt.Fprintf(w, " Strategy: %s%s%s | Gateway: %s\n", clrBold, r.Strategy, clrReset, gwStr)
	fmt.Fprintf(w, " Analysis Window: past %.1fh (Total: %d generation requests)\n\n",
		float64(r.WindowSeconds)/3600.0, r.WindowRequests)

	fmt.Fprintf(w, " %sAccount Load Distribution & Quotas:%s\n", clrBold, clrReset)
	for _, acc := range r.Accounts {
		nextMarker := ""
		if acc.IsNext {
			nextMarker = " " + clrGreen + "[Next ⚡]" + clrReset
		}

		pctFrac := acc.WindowPercent / 100.0
		bar := quota.RenderBlockProgressBar(&pctFrac, 15)

		q5Str := "--"
		if acc.Quota5HPercent != nil {
			q5Str = fmt.Sprintf("%.0f%%", *acc.Quota5HPercent)
		}
		qwStr := "--"
		if acc.QuotaWkPercent != nil {
			qwStr = fmt.Sprintf("%.0f%%", *acc.QuotaWkPercent)
		}

		statusColor := clrReset
		switch acc.Status {
		case "Ready":
			statusColor = clrGreen
		case "Exhausted":
			statusColor = clrRed
		case "Restricted":
			statusColor = clrRed
		case "Cooldown":
			statusColor = clrYellow
		case "Disabled":
			statusColor = clrDim
		}

		fmt.Fprintf(w, "  • %-8s %s %5.1f%% (%3d reqs)  5H: %4s  WK: %4s  %s%s%s%s\n",
			acc.Name, bar, acc.WindowPercent, acc.WindowRequests, q5Str, qwStr, statusColor, acc.Status, clrReset, nextMarker)
	}

	fmt.Fprintf(w, "\n %sRotation & Balance Analysis:%s\n", clrBold, clrReset)
	balanceColor := clrGreen
	if r.BalanceStatus == "SKEWED" {
		balanceColor = clrYellow
	} else if r.BalanceStatus == "MONOPOLIZED" {
		balanceColor = clrRed
	}

	fmt.Fprintf(w, "  • Workload Balance:   %s%s (Index: %.2f / 1.0)%s\n",
		balanceColor, r.BalanceStatus, r.BalanceIndex, clrReset)
	if r.DominantAccount != "" && r.ActiveAccounts > 1 && r.BalanceStatus != "BALANCED" && r.BalanceStatus != "OPTIMAL" {
		domName := r.DominantAccount
		for _, a := range r.Accounts {
			if a.ID == r.DominantAccount && a.Name != "" && a.Name != a.ID {
				domName = fmt.Sprintf("%s (%s)", a.Name, a.ID)
				break
			}
		}
		fmt.Fprintf(w, "  • Dominant Account:   %s%s%s\n", clrYellow, domName, clrReset)
	}
	if r.Gateway.Uptime != "" {
		fmt.Fprintf(w, "  • Gateway Resilience: %d routed | %d failovers (since start, %s)\n",
			r.Gateway.RoutingDecisions, r.Gateway.FailoversTotal, r.Gateway.Uptime)
	} else {
		fmt.Fprintf(w, "  • Gateway Resilience: %d routed | %d failovers\n",
			r.Gateway.RoutingDecisions, r.Gateway.FailoversTotal)
	}

	verdictColor := clrGreen
	switch r.AIVerdict.Status {
	case "OPTIMAL", "HEALTHY":
		verdictColor = clrGreen
	case "WARNING":
		verdictColor = clrYellow
	default:
		verdictColor = clrRed
	}

	fmt.Fprintf(w, "\n %sAI Diagnostic Verdict:%s\n", clrBold, clrReset)
	fmt.Fprintf(w, "  %s[%s]%s %s\n", verdictColor, r.AIVerdict.Status, clrReset, r.AIVerdict.Summary)

	if len(r.AIVerdict.Issues) > 0 {
		fmt.Fprintf(w, "  %sDetected Issues:%s\n", clrYellow, clrReset)
		for _, issue := range r.AIVerdict.Issues {
			fmt.Fprintf(w, "    ⚠ %s\n", issue)
		}
	}

	if len(r.AIVerdict.Recommendations) > 0 {
		fmt.Fprintf(w, "  %sRecommendations:%s\n", clrCyan, clrReset)
		for _, rec := range r.AIVerdict.Recommendations {
			fmt.Fprintf(w, "    → %s\n", rec)
		}
	}

	fmt.Fprintf(w, "====================================================================\n\n")
}
