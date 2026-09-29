package proxy

import (
	"sort"
	"sync"

	"github.com/vlxlv/agy-pool-go/internal/storage"
)

var (
	confirmationMu     sync.Mutex
	confirmationClaims = make(map[string]bool)
)

// TryClaimVerificationConfirmation attempts to claim an in-flight confirmation probe for the given accountID.
// Returns true if the claim was granted, or false if already claimed or invalid.
func TryClaimVerificationConfirmation(accountID string) bool {
	if accountID == "" {
		return false
	}
	confirmationMu.Lock()
	defer confirmationMu.Unlock()
	if confirmationClaims[accountID] {
		return false
	}
	confirmationClaims[accountID] = true
	return true
}

// ReleaseVerificationConfirmation releases an in-flight confirmation claim for the given accountID.
func ReleaseVerificationConfirmation(accountID string) {
	if accountID == "" {
		return
	}
	confirmationMu.Lock()
	defer confirmationMu.Unlock()
	delete(confirmationClaims, accountID)
}

// ClearAllVerificationConfirmationClaims clears all active claims (primarily for tests).
func ClearAllVerificationConfirmationClaims() {
	confirmationMu.Lock()
	defer confirmationMu.Unlock()
	confirmationClaims = make(map[string]bool)
}

// IsVerificationConfirmationClaimed reports whether a confirmation claim is currently active for accountID.
func IsVerificationConfirmationClaimed(accountID string) bool {
	if accountID == "" {
		return false
	}
	confirmationMu.Lock()
	defer confirmationMu.Unlock()
	return confirmationClaims[accountID]
}

// SelectAndClaimConfirmationCandidate inspects accounts for StatusVerificationPending,
// orders them deterministically (oldest UpdatedAt first, breaking ties by ID ascending),
// and atomically claims at most ONE unclaimed candidate.
// Returns the claimed account, or nil if no candidate is available or all are claimed.
func SelectAndClaimConfirmationCandidate(accounts []*storage.Account) *storage.Account {
	if len(accounts) == 0 {
		return nil
	}

	var pending []*storage.Account
	for _, a := range accounts {
		if a != nil && a.Status == storage.StatusVerificationPending {
			pending = append(pending, a)
		}
	}
	if len(pending) == 0 {
		return nil
	}

	// Deterministic / fair ordering: oldest UpdatedAt first (FIFO), breaking ties by ID ascending.
	sort.SliceStable(pending, func(i, j int) bool {
		upI, upJ := int64(0), int64(0)
		if pending[i].UpdatedAt != nil {
			upI = *pending[i].UpdatedAt
		}
		if pending[j].UpdatedAt != nil {
			upJ = *pending[j].UpdatedAt
		}
		if upI != upJ {
			return upI < upJ
		}
		return pending[i].ID < pending[j].ID
	})

	for _, cand := range pending {
		if TryClaimVerificationConfirmation(cand.ID) {
			return cand
		}
	}
	return nil
}
