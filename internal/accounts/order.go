package accounts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

// ANSI Color Codes
const (
	clrReset  = "\033[0m"
	clrBold   = "\033[1m"
	clrDim    = "\033[2m"
	clrGreen  = "\033[32m"
	clrYellow = "\033[33m"
	clrCyan   = "\033[36m"
	clrRed    = "\033[31m"
)

// MoveResult holds the outcome of a MoveAccount operation.
type MoveResult struct {
	OrigIndex int
	DestIndex int
	Account   *storage.Account
	Accounts  []*storage.Account
	ActiveID  *string
	IsSame    bool
}

// SwapResult holds the outcome of a SwapAccounts operation.
type SwapResult struct {
	Index1   int
	Index2   int
	Account1 *storage.Account
	Account2 *storage.Account
	Accounts []*storage.Account
	ActiveID *string
	IsSame   bool
}

// SortResult holds the outcome of a SortAccounts operation.
type SortResult struct {
	Criterion string
	Reversed  bool
	Accounts  []*storage.Account
	ActiveID  *string
}

// ReorderResult holds the outcome of a ReorderAccounts operation.
type ReorderResult struct {
	Accounts []*storage.Account
	ActiveID *string
}

// MoveAccount moves an account to a new position in the pool.
// Destination can be:
//   - 1-based index (1..N)
//   - 'top' / 'first' / 'head' / 'begin'
//   - 'bottom' / 'last' / 'tail' / 'end'
//   - 'up' / '-1' / '-N'
//   - 'down' / '+1' / '+N'
//   - another account index/name/ID (moves to that account's position)
func MoveAccount(target, destination string) (*MoveResult, error) {
	if strings.TrimSpace(target) == "" || strings.TrimSpace(destination) == "" {
		return nil, errors.New("both target account and destination position must be specified")
	}

	var res *MoveResult
	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		if pool == nil || len(pool.Accounts) == 0 {
			return errors.New("account pool is empty")
		}
		origIdx := ResolveAccountIndex(pool.Accounts, target)
		if origIdx < 0 {
			return fmt.Errorf("account '%s' not found", target)
		}

		n := len(pool.Accounts)
		destStr := strings.ToLower(strings.TrimSpace(destination))
		var destIdx int

		switch destStr {
		case "top", "first", "head", "begin":
			destIdx = 0
		case "bottom", "last", "tail", "end":
			destIdx = n - 1
		case "up", "-1":
			destIdx = origIdx - 1
			if destIdx < 0 {
				destIdx = 0
			}
		case "down", "+1":
			destIdx = origIdx + 1
			if destIdx >= n {
				destIdx = n - 1
			}
		default:
			if strings.HasPrefix(destStr, "+") && isDigits(destStr[1:]) {
				delta, _ := strconv.Atoi(destStr[1:])
				destIdx = origIdx + delta
				if destIdx >= n {
					destIdx = n - 1
				}
			} else if strings.HasPrefix(destStr, "-") && isDigits(destStr[1:]) {
				delta, _ := strconv.Atoi(destStr[1:])
				destIdx = origIdx - delta
				if destIdx < 0 {
					destIdx = 0
				}
			} else if isDigits(destStr) {
				d, _ := strconv.Atoi(destStr)
				if d < 1 || d > n {
					return fmt.Errorf("destination position %d is out of range. Must be between 1 and %d", d, n)
				}
				destIdx = d - 1
			} else {
				otherIdx := ResolveAccountIndex(pool.Accounts, destination)
				if otherIdx >= 0 {
					destIdx = otherIdx
				} else {
					return fmt.Errorf("invalid destination '%s'. Use a 1-based number (1..%d) or top/bottom/up/down", destination, n)
				}
			}
		}

		if origIdx == destIdx {
			res = &MoveResult{
				OrigIndex: origIdx,
				DestIndex: destIdx,
				Account:   pool.Accounts[origIdx],
				Accounts:  pool.Accounts,
				ActiveID:  pool.ActiveAccountID,
				IsSame:    true,
			}
			return nil
		}

		item := pool.Accounts[origIdx]
		// Remove item at origIdx
		pool.Accounts = append(pool.Accounts[:origIdx], pool.Accounts[origIdx+1:]...)
		// Insert item at destIdx
		newAccounts := make([]*storage.Account, 0, n)
		newAccounts = append(newAccounts, pool.Accounts[:destIdx]...)
		newAccounts = append(newAccounts, item)
		newAccounts = append(newAccounts, pool.Accounts[destIdx:]...)
		pool.Accounts = newAccounts

		// Keep active account valid
		hasActive := false
		if pool.ActiveAccountID != nil {
			for _, a := range pool.Accounts {
				if a != nil && a.ID == *pool.ActiveAccountID {
					hasActive = true
					break
				}
			}
		}
		if !hasActive && len(pool.Accounts) > 0 {
			pool.ActiveAccountID = &pool.Accounts[0].ID
		}

		res = &MoveResult{
			OrigIndex: origIdx,
			DestIndex: destIdx,
			Account:   item,
			Accounts:  pool.Accounts,
			ActiveID:  pool.ActiveAccountID,
			IsSame:    false,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return res, nil
}

// SwapAccounts swaps the positions of two accounts in the pool.
func SwapAccounts(target1, target2 string) (*SwapResult, error) {
	if strings.TrimSpace(target1) == "" || strings.TrimSpace(target2) == "" {
		return nil, errors.New("two target accounts must be specified for swap")
	}

	var res *SwapResult
	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		if pool == nil || len(pool.Accounts) == 0 {
			return errors.New("account pool is empty")
		}
		idx1 := ResolveAccountIndex(pool.Accounts, target1)
		if idx1 < 0 {
			return fmt.Errorf("account '%s' not found", target1)
		}
		idx2 := ResolveAccountIndex(pool.Accounts, target2)
		if idx2 < 0 {
			return fmt.Errorf("account '%s' not found", target2)
		}

		if idx1 == idx2 {
			res = &SwapResult{
				Index1:   idx1,
				Index2:   idx2,
				Account1: pool.Accounts[idx1],
				Account2: pool.Accounts[idx2],
				Accounts: pool.Accounts,
				ActiveID: pool.ActiveAccountID,
				IsSame:   true,
			}
			return nil
		}

		pool.Accounts[idx1], pool.Accounts[idx2] = pool.Accounts[idx2], pool.Accounts[idx1]

		res = &SwapResult{
			Index1:   idx1,
			Index2:   idx2,
			Account1: pool.Accounts[idx2], // originally at idx1, now at idx2
			Account2: pool.Accounts[idx1], // originally at idx2, now at idx1
			Accounts: pool.Accounts,
			ActiveID: pool.ActiveAccountID,
			IsSame:   false,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return res, nil
}

// SortAccounts sorts the account pool by the given criteria.
func SortAccounts(by string, reverse bool, refresh bool) (*SortResult, error) {
	criterion := strings.ToLower(strings.TrimSpace(by))
	if criterion == "" {
		criterion = "quota"
	}
	validCriteria := map[string]bool{
		"quota": true, "hits": true, "usage": true, "name": true,
		"email": true, "status": true, "id": true, "created": true, "time": true,
	}
	if !validCriteria[criterion] {
		return nil, fmt.Errorf("unknown sort criterion '%s'. Available criteria: quota, hits, name, email, status, id, created", by)
	}

	if refresh && criterion == "quota" {
		pool, err := storage.LoadPool()
		if err == nil && pool != nil && len(pool.Accounts) > 0 {
			prober, _ := getQuotaProber()
			if prober != nil {
				var wg sync.WaitGroup
				ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
				defer cancel()
				for _, acc := range pool.Accounts {
					if acc == nil {
						continue
					}
					wg.Add(1)
					go func(a *storage.Account) {
						defer wg.Done()
						select {
						case <-ctx.Done():
							return
						default:
							_, _ = prober(a)
						}
					}(acc)
				}
				wg.Wait()
			}
		}
	}

	nowTS := time.Now().Unix()
	var isReverse bool
	if criterion == "quota" || criterion == "hits" || criterion == "usage" {
		isReverse = !reverse
	} else {
		isReverse = reverse
	}

	var res *SortResult
	err := storage.PoolTransaction(func(pool *storage.Pool) error {
		if pool == nil || len(pool.Accounts) == 0 {
			return errors.New("account pool is empty")
		}

		sort.SliceStable(pool.Accounts, func(i, j int) bool {
			cmp := compareAccounts(pool.Accounts[i], pool.Accounts[j], criterion, nowTS)
			if isReverse {
				return cmp > 0
			}
			return cmp < 0
		})

		hasActive := false
		if pool.ActiveAccountID != nil {
			for _, a := range pool.Accounts {
				if a != nil && a.ID == *pool.ActiveAccountID {
					hasActive = true
					break
				}
			}
		}
		if !hasActive && len(pool.Accounts) > 0 {
			pool.ActiveAccountID = &pool.Accounts[0].ID
		}

		res = &SortResult{
			Criterion: criterion,
			Reversed:  reverse,
			Accounts:  pool.Accounts,
			ActiveID:  pool.ActiveAccountID,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return res, nil
}

// ReorderAccounts permutes all accounts according to the provided sequence of indices/names.
func ReorderAccounts(orderArgs []string) (*ReorderResult, error) {
	var flattened []string
	for _, arg := range orderArgs {
		parts := strings.Fields(strings.ReplaceAll(arg, ",", " "))
		flattened = append(flattened, parts...)
	}

	pool, err := storage.LoadPool()
	if err != nil || pool == nil || len(pool.Accounts) == 0 {
		return nil, errors.New("account pool is empty")
	}

	n := len(pool.Accounts)
	if len(flattened) != n {
		return nil, fmt.Errorf("reorder sequence count (%d) does not match pool account count (%d)", len(flattened), n)
	}

	resolvedIndices := make([]int, 0, n)
	seen := make(map[int]bool)
	for _, item := range flattened {
		idx := ResolveAccountIndex(pool.Accounts, item)
		if idx < 0 {
			return nil, fmt.Errorf("account '%s' not found or invalid", item)
		}
		if seen[idx] {
			accName := DisplayAccountName(pool.Accounts[idx])
			return nil, fmt.Errorf("duplicate account specified in reorder sequence: [%d] %s", idx+1, accName)
		}
		seen[idx] = true
		resolvedIndices = append(resolvedIndices, idx)
	}

	var res *ReorderResult
	err = storage.PoolTransaction(func(p *storage.Pool) error {
		if len(p.Accounts) != n {
			return errors.New("account pool changed during reorder, please retry")
		}
		newAccs := make([]*storage.Account, n)
		for i, idx := range resolvedIndices {
			newAccs[i] = p.Accounts[idx]
		}
		p.Accounts = newAccs

		hasActive := false
		if p.ActiveAccountID != nil {
			for _, a := range p.Accounts {
				if a != nil && a.ID == *p.ActiveAccountID {
					hasActive = true
					break
				}
			}
		}
		if !hasActive && len(p.Accounts) > 0 {
			p.ActiveAccountID = &p.Accounts[0].ID
		}

		res = &ReorderResult{
			Accounts: p.Accounts,
			ActiveID: p.ActiveAccountID,
		}
		return nil
	})

	if err != nil {
		return nil, err
	}
	return res, nil
}

// PrintAccountOrderPreview prints a clean, concise list preview of account ordering.
func PrintAccountOrderPreview(w io.Writer, accounts []*storage.Account, highlightIndices []int, activeID *string, showEmailOpt ...bool) {
	showEmail := false
	if len(showEmailOpt) > 0 {
		showEmail = showEmailOpt[0]
	} else {
		showEmail = IsShowEmailEnabled(nil)
	}

	isHighlighted := make(map[int]bool)
	for _, idx := range highlightIndices {
		isHighlighted[idx] = true
	}

	fmt.Fprintf(w, "\n%sAccount Pool Order (%d accounts):%s\n", clrBold, len(accounts), clrReset)
	for i, acc := range accounts {
		idx0 := i
		accID := acc.ID
		isActive := activeID != nil && accID == *activeID
		activeTag := fmt.Sprintf("%s          %s", clrDim, clrReset)
		if isActive {
			activeTag = fmt.Sprintf("%s[* Active]%s", clrGreen, clrReset)
		}

		dispName := DisplayAccountName(acc, showEmail)
		hits := acc.GetHits()
		q5Frac, _ := displayQuotaFractions(acc.LastQuota)
		var quotaStr string
		if q5Frac != nil {
			g5Bar := renderCompactBar(q5Frac, 8)
			quotaStr = fmt.Sprintf("  Quota: %s", g5Bar)
		}
		var hitsStr string
		if hits > 0 {
			hitsStr = fmt.Sprintf("  Hits: %d", hits)
		}

		var prefix, nameStr string
		if isHighlighted[idx0] {
			prefix = fmt.Sprintf("%s➜ [%2d]%s", clrCyan, i+1, clrReset)
			nameStr = fmt.Sprintf("%s%s%s%s", clrBold, clrCyan, dispName, clrReset)
		} else {
			prefix = fmt.Sprintf("  [%2d]", i+1)
			nameStr = dispName
		}

		fmt.Fprintf(w, " %s %s %s%s%s\n", prefix, activeTag, nameStr, quotaStr, hitsStr)
	}
	fmt.Fprintln(w)
}

func displayQuotaFractions(q *storage.QuotaState) (*float64, *float64) {
	if q == nil {
		return nil, nil
	}
	var q5, qw *float64
	if q.Gemini5H != nil && q.Gemini5H.Fraction != nil {
		q5 = q.Gemini5H.Fraction
	} else if q.RemainingFraction != nil {
		q5 = q.RemainingFraction
	}
	if q.GeminiWeekly != nil && q.GeminiWeekly.Fraction != nil {
		qw = q.GeminiWeekly.Fraction
	}
	return q5, qw
}

func renderCompactBar(fraction *float64, width int) string {
	if width <= 0 {
		width = 8
	}
	if fraction == nil {
		return fmt.Sprintf("[%s%s%s] N/A", clrDim, strings.Repeat("─", width), clrReset)
	}
	f := *fraction
	if f < 0.0 {
		f = 0.0
	} else if f > 1.0 {
		f = 1.0
	}
	filled := int(math.Round(f * float64(width)))
	if filled > width {
		filled = width
	}
	empty := width - filled
	pct := fmt.Sprintf("%d%%", int(math.Round(f*100.0)))
	color := clrRed
	if f > 0.4 {
		color = clrGreen
	} else if f > 0.15 {
		color = clrYellow
	}
	filledBar := strings.Repeat("━", filled)
	emptyBar := strings.Repeat("─", empty)
	return fmt.Sprintf("[%s%s%s%s%s%s] %s%s%s",
		color, filledBar, clrReset,
		clrDim, emptyBar, clrReset,
		color, pct, clrReset)
}

// Comparison helpers for sorting

type naturalIDKey struct {
	hasNum bool
	num    int
	raw    string
}

func getNaturalIDKey(id string) naturalIDKey {
	start := -1
	end := -1
	for i, r := range id {
		if r >= '0' && r <= '9' {
			if start == -1 {
				start = i
			}
			end = i + 1
		} else if start != -1 {
			break
		}
	}
	if start != -1 {
		if n, err := strconv.Atoi(id[start:end]); err == nil {
			return naturalIDKey{hasNum: true, num: n, raw: id}
		}
	}
	return naturalIDKey{hasNum: false, num: 0, raw: id}
}

func compareNaturalID(k1, k2 naturalIDKey) int {
	if k1.hasNum && k2.hasNum {
		if k1.num != k2.num {
			if k1.num < k2.num {
				return -1
			}
			return 1
		}
		return strings.Compare(k1.raw, k2.raw)
	}
	if k1.hasNum && !k2.hasNum {
		return -1
	}
	if !k1.hasNum && k2.hasNum {
		return 1
	}
	return strings.Compare(k1.raw, k2.raw)
}

func accountStatusRank(acc *storage.Account, nowTS int64) int {
	if acc == nil {
		return 6
	}
	if acc.Status == storage.StatusAuthError {
		return 5
	}
	if acc.Status == storage.StatusValidationRequired {
		return 4
	}
	q5, qw := displayQuotaFractions(acc.LastQuota)
	isExhausted := (q5 != nil && *q5 <= config.DepletedThreshold) || (qw != nil && *qw <= config.DepletedThreshold)
	if isExhausted {
		return 3
	}
	if acc.RateLimitedUntil != nil && *acc.RateLimitedUntil > float64(nowTS) {
		return 2
	}
	return 1
}

func compareAccounts(a, b *storage.Account, criterion string, nowTS int64) int {
	natA := getNaturalIDKey(a.ID)
	natB := getNaturalIDKey(b.ID)

	switch criterion {
	case "quota":
		penaltyA := 0.0
		if a.Status == storage.StatusValidationRequired || a.Status == storage.StatusAuthError {
			penaltyA = -1.0
		}
		penaltyB := 0.0
		if b.Status == storage.StatusValidationRequired || b.Status == storage.StatusAuthError {
			penaltyB = -1.0
		}
		if penaltyA != penaltyB {
			if penaltyA < penaltyB {
				return -1
			}
			return 1
		}

		q5A, qwA := displayQuotaFractions(a.LastQuota)
		q5B, qwB := displayQuotaFractions(b.LastQuota)
		val5A := 0.0
		if q5A != nil {
			val5A = *q5A
		}
		val5B := 0.0
		if q5B != nil {
			val5B = *q5B
		}
		if math.Abs(val5A-val5B) > 1e-6 {
			if val5A < val5B {
				return -1
			}
			return 1
		}

		valWA := 1.0
		if qwA != nil {
			valWA = *qwA
		}
		valWB := 1.0
		if qwB != nil {
			valWB = *qwB
		}
		if math.Abs(valWA-valWB) > 1e-6 {
			if valWA < valWB {
				return -1
			}
			return 1
		}

		hitsA := a.GetHits()
		hitsB := b.GetHits()
		if hitsA != hitsB {
			// Higher hits means smaller quota preference (-hits)
			if hitsA > hitsB {
				return -1
			}
			return 1
		}
		return compareNaturalID(natA, natB)

	case "hits", "usage":
		hitsA := a.GetHits()
		hitsB := b.GetHits()
		if hitsA != hitsB {
			if hitsA < hitsB {
				return -1
			}
			return 1
		}
		q5A, _ := displayQuotaFractions(a.LastQuota)
		q5B, _ := displayQuotaFractions(b.LastQuota)
		val5A := 0.0
		if q5A != nil {
			val5A = *q5A
		}
		val5B := 0.0
		if q5B != nil {
			val5B = *q5B
		}
		if math.Abs(val5A-val5B) > 1e-6 {
			if val5A < val5B {
				return -1
			}
			return 1
		}
		return compareNaturalID(natA, natB)

	case "name":
		nameA := strings.ToLower(strings.TrimSpace(DisplayAccountName(a)))
		nameB := strings.ToLower(strings.TrimSpace(DisplayAccountName(b)))
		if cmp := strings.Compare(nameA, nameB); cmp != 0 {
			return cmp
		}
		return compareNaturalID(natA, natB)

	case "email":
		emailA := strings.ToLower(strings.TrimSpace(a.Email))
		emailB := strings.ToLower(strings.TrimSpace(b.Email))
		if cmp := strings.Compare(emailA, emailB); cmp != 0 {
			return cmp
		}
		return compareNaturalID(natA, natB)

	case "status":
		rankA := accountStatusRank(a, nowTS)
		rankB := accountStatusRank(b, nowTS)
		if rankA != rankB {
			if rankA < rankB {
				return -1
			}
			return 1
		}
		// Tie breakers: higher quota frac, lower hits, natural ID
		q5A, _ := displayQuotaFractions(a.LastQuota)
		q5B, _ := displayQuotaFractions(b.LastQuota)
		val5A := 0.0
		if q5A != nil {
			val5A = *q5A
		}
		val5B := 0.0
		if q5B != nil {
			val5B = *q5B
		}
		if math.Abs(val5A-val5B) > 1e-6 {
			if val5A > val5B {
				return -1
			}
			return 1
		}
		hitsA := a.GetHits()
		hitsB := b.GetHits()
		if hitsA != hitsB {
			if hitsA < hitsB {
				return -1
			}
			return 1
		}
		return compareNaturalID(natA, natB)

	case "id":
		return compareNaturalID(natA, natB)

	case "created", "time":
		tsA := int64(0)
		if a.CreatedAt != nil {
			tsA = *a.CreatedAt
		} else if a.UpdatedAt != nil {
			tsA = *a.UpdatedAt
		}
		tsB := int64(0)
		if b.CreatedAt != nil {
			tsB = *b.CreatedAt
		} else if b.UpdatedAt != nil {
			tsB = *b.UpdatedAt
		}
		if tsA != tsB {
			if tsA < tsB {
				return -1
			}
			return 1
		}
		return compareNaturalID(natA, natB)
	}

	return 0
}
