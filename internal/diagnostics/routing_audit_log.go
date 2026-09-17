package diagnostics

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ParsedLogEntry contains extracted fields from a daemon log line.
type ParsedLogEntry struct {
	Timestamp      time.Time
	Type           string // "PROXY", "PROXY ERROR", "FAILOVER"
	Path           string
	AccountName    string
	Status         int
	FailoverReason string
	IsGeneration   bool
	HasAuditMeta   bool
	Strategy       string
	Classification string
	HealthyCount   int
	ReserveCount   int
}

// ParseLogLine parses a log line produced by the agy-pool proxy.
func ParseLogLine(line string) (*ParsedLogEntry, bool) {
	line = strings.TrimSpace(line)
	if len(line) < 22 || line[0] != '[' || line[20] != ']' {
		return nil, false
	}

	ts, err := time.ParseInLocation("2006-01-02 15:04:05", line[1:20], time.Local)
	if err != nil {
		return nil, false
	}

	rest := strings.TrimSpace(line[21:])
	tagEnd := strings.Index(rest, "] ")
	if tagEnd == -1 || rest[0] != '[' {
		return nil, false
	}

	tag := rest[1:tagEnd]
	msg := strings.TrimSpace(rest[tagEnd+2:])

	switch tag {
	case "PROXY":
		// Format: METHOD PATH -> ACCOUNT (Status: STATUS) [audit: ...]
		arrowIdx := strings.Index(msg, " -> ")
		if arrowIdx == -1 {
			return nil, false
		}
		methodPath := strings.TrimSpace(msg[:arrowIdx])
		parts := strings.SplitN(methodPath, " ", 2)
		path := ""
		if len(parts) == 2 {
			path = parts[1]
		}

		afterArrow := strings.TrimSpace(msg[arrowIdx+4:])
		statusIdx := strings.Index(afterArrow, " (Status: ")
		if statusIdx == -1 {
			return nil, false
		}
		accountName := strings.TrimSpace(afterArrow[:statusIdx])

		remainder := afterArrow[statusIdx+10:]
		statusEnd := strings.Index(remainder, ")")
		if statusEnd == -1 {
			return nil, false
		}
		status, err := strconv.Atoi(remainder[:statusEnd])
		if err != nil {
			return nil, false
		}

		strat := ""
		class := ""
		hCount := 0
		rCount := 0
		hasMeta := false

		auditIdx := strings.Index(remainder, "[audit:")
		if auditIdx != -1 {
			metaStr := strings.Trim(remainder[auditIdx:], "[]")
			for _, token := range strings.Fields(metaStr) {
				if strings.HasPrefix(token, "strategy=") {
					strat = strings.TrimPrefix(token, "strategy=")
				} else if strings.HasPrefix(token, "tier=") {
					class = strings.TrimPrefix(token, "tier=")
				} else if strings.HasPrefix(token, "healthy=") {
					hCount, _ = strconv.Atoi(strings.TrimPrefix(token, "healthy="))
				} else if strings.HasPrefix(token, "reserve=") {
					rCount, _ = strconv.Atoi(strings.TrimPrefix(token, "reserve="))
				}
			}
			hasMeta = true
		}

		isGen := strings.Contains(path, "generateContent") || strings.Contains(path, "streamGenerateContent")
		return &ParsedLogEntry{
			Timestamp:      ts,
			Type:           "PROXY",
			Path:           path,
			AccountName:    accountName,
			Status:         status,
			IsGeneration:   isGen,
			HasAuditMeta:   hasMeta,
			Strategy:       strat,
			Classification: class,
			HealthyCount:   hCount,
			ReserveCount:   rCount,
		}, true

	case "PROXY ERROR":
		// Format: METHOD PATH -> ACCOUNT HTTP STATUS [audit: ...]
		arrowIdx := strings.Index(msg, " -> ")
		if arrowIdx == -1 {
			return nil, false
		}
		methodPath := strings.TrimSpace(msg[:arrowIdx])
		parts := strings.SplitN(methodPath, " ", 2)
		path := ""
		if len(parts) == 2 {
			path = parts[1]
		}

		afterArrow := strings.TrimSpace(msg[arrowIdx+4:])
		httpIdx := strings.Index(afterArrow, " HTTP ")
		if httpIdx == -1 {
			return nil, false
		}
		accountName := strings.TrimSpace(afterArrow[:httpIdx])

		remainder := strings.TrimSpace(afterArrow[httpIdx+6:])
		statusStr := remainder
		auditIdx := strings.Index(remainder, " [audit:")
		if auditIdx != -1 {
			statusStr = remainder[:auditIdx]
		}
		status, err := strconv.Atoi(statusStr)
		if err != nil {
			return nil, false
		}

		strat := ""
		class := ""
		hCount := 0
		rCount := 0
		hasMeta := false

		if auditIdx != -1 {
			metaStr := strings.Trim(remainder[auditIdx:], "[]")
			for _, token := range strings.Fields(metaStr) {
				if strings.HasPrefix(token, "strategy=") {
					strat = strings.TrimPrefix(token, "strategy=")
				} else if strings.HasPrefix(token, "tier=") {
					class = strings.TrimPrefix(token, "tier=")
				} else if strings.HasPrefix(token, "healthy=") {
					hCount, _ = strconv.Atoi(strings.TrimPrefix(token, "healthy="))
				} else if strings.HasPrefix(token, "reserve=") {
					rCount, _ = strconv.Atoi(strings.TrimPrefix(token, "reserve="))
				}
			}
			hasMeta = true
		}

		isGen := strings.Contains(path, "generateContent") || strings.Contains(path, "streamGenerateContent")
		return &ParsedLogEntry{
			Timestamp:      ts,
			Type:           "PROXY ERROR",
			Path:           path,
			AccountName:    accountName,
			Status:         status,
			IsGeneration:   isGen,
			HasAuditMeta:   hasMeta,
			Strategy:       strat,
			Classification: class,
			HealthyCount:   hCount,
			ReserveCount:   rCount,
		}, true

	case "FAILOVER":
		// Format: Account ACCOUNT hit rate limit... or Account ACCOUNT requires...
		reason := "failover"
		accountName := ""

		tokens := strings.Fields(msg)
		if len(tokens) >= 2 && tokens[0] == "Account" {
			accountName = tokens[1]
		}

		if strings.Contains(msg, "rate limit/quota") {
			reason = "QuotaExhausted"
		} else if strings.Contains(msg, "verification") {
			reason = "ValidationError"
		} else if strings.Contains(msg, "auth/permission") {
			reason = "AuthFailure"
		}

		return &ParsedLogEntry{
			Timestamp:      ts,
			Type:           "FAILOVER",
			AccountName:    accountName,
			FailoverReason: reason,
		}, true

	default:
		return nil, false
	}
}

func isRotatedOrTruncated(f *os.File, path string) bool {
	if f == nil {
		return true
	}
	fi, err := f.Stat()
	if err != nil {
		return true
	}
	st, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !os.SameFile(fi, st) {
		return true
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err == nil && st.Size() < pos {
		return true
	}
	return false
}

func (a *Auditor) runLogFollower(ctx context.Context, stdout, stderr io.Writer, pool *storage.Pool) error {
	logFile := a.options.LogPath
	if logFile == "" {
		logFile = config.GetLogFile()
	}

	// Wait up to 2 seconds for log file to exist if not present yet
	for i := 0; i < 20; i++ {
		if _, err := os.Stat(logFile); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			a.summary.Duration = time.Since(a.startTime)
			a.PrintSummary(stdout)
			_ = a.WriteReport()
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}

	f, err := os.Open(logFile)
	if err != nil {
		if !a.options.JSONMode {
			fmt.Fprintf(stderr, "Log file does not exist yet: %s. Waiting for events...\n", logFile)
		}
	} else {
		defer f.Close()
		if !a.options.FromStart {
			_, _ = f.Seek(0, io.SeekEnd)
		}
	}

	currentAttempt := 1
	lastFailoverReason := ""

	var reader *bufio.Reader
	if f != nil {
		reader = bufio.NewReader(f)
	}

	for {
		select {
		case <-ctx.Done():
			a.summary.Duration = time.Since(a.startTime)
			a.PrintSummary(stdout)
			_ = a.WriteReport()
			return nil
		default:
		}

		if f == nil {
			newF, err := os.Open(logFile)
			if err == nil {
				f = newF
				defer f.Close()
				reader = bufio.NewReader(f)
			} else {
				time.Sleep(200 * time.Millisecond)
				continue
			}
		}

		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				// Check for log rotation or truncation
				if isRotatedOrTruncated(f, logFile) {
					newF, reopenErr := os.Open(logFile)
					if reopenErr == nil {
						_ = f.Close()
						f = newF
						reader = bufio.NewReader(f)
						continue
					}
				}
				time.Sleep(100 * time.Millisecond)
				continue
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}

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
}
