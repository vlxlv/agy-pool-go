package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

type evalSchedulerInput struct {
	Strategy   string             `json:"strategy"`
	Now        float64            `json:"now"`
	Pool       *storage.Pool      `json:"pool"`
	Candidates []*storage.Account `json:"candidates"`
}

type evalSchedulerOutput struct {
	OrderedIDs []string `json:"ordered_ids"`
}

type evalCapacityInput struct {
	Account *storage.Account `json:"account"`
	Now     float64          `json:"now"`
}

type evalCapacityOutput struct {
	CapacityState quota.CapacityState `json:"capacity_state"`
	FreshnessInfo quota.FreshnessInfo `json:"freshness_info"`
	FreshnessRank int                 `json:"freshness_rank"`
	RefreshNeeded bool                `json:"refresh_needed"`
}

func readStdinJSON(v any) error {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("failed to read stdin: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("failed to parse input JSON: %w", err)
	}
	return nil
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			fmt.Printf("agy-pool-go %s\n", config.Version)
			return

		case "eval-scheduler":
			var input evalSchedulerInput
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			ordered := scheduler.OrderCandidates(input.Candidates, input.Strategy, input.Pool, input.Now)
			ids := make([]string, 0, len(ordered))
			for _, a := range ordered {
				ids = append(ids, a.ID)
			}
			out, _ := json.Marshal(evalSchedulerOutput{OrderedIDs: ids})
			fmt.Println(string(out))
			return

		case "eval-capacity":
			var input evalCapacityInput
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			capState := quota.ComputeCapacityState(input.Account, input.Now)
			fresh := quota.Freshness(input.Account, input.Now)
			rank := quota.FreshnessRank(input.Account, input.Now)
			needed := quota.RefreshNeeded(input.Account, input.Now)
			out, _ := json.Marshal(evalCapacityOutput{
				CapacityState: capState,
				FreshnessInfo: fresh,
				FreshnessRank: rank,
				RefreshNeeded: needed,
			})
			fmt.Println(string(out))
			return

		case "eval-account-target":
			var input struct {
				Accounts []*storage.Account `json:"accounts"`
				Target   string             `json:"target"`
			}
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			found := accounts.FindAccountByTarget(input.Accounts, input.Target)
			var foundID *string
			if found != nil {
				foundID = &found.ID
			}
			out, _ := json.Marshal(map[string]any{"found_id": foundID})
			fmt.Println(string(out))
			return

		case "eval-display-name":
			var input struct {
				Account *storage.Account `json:"account"`
			}
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			name := accounts.DisplayAccountName(input.Account)
			out, _ := json.Marshal(map[string]any{"display_name": name})
			fmt.Println(string(out))
			return

		case "eval-jwt":
			var input struct {
				JWT string `json:"jwt"`
			}
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			claims := auth.DecodeJWTPayload(input.JWT)
			out, _ := json.Marshal(map[string]any{"claims": claims})
			fmt.Println(string(out))
			return

		case "eval-error-classifier":
			var input struct {
				Status int    `json:"status"`
				Body   string `json:"body"`
			}
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			bodyBytes := []byte(input.Body)
			isVal := auth.IsValidationError(input.Status, bodyBytes)
			valURL := auth.ExtractValidationURL(bodyBytes)
			isAuth := auth.IsAuthError(input.Status, bodyBytes)
			out, _ := json.Marshal(map[string]any{
				"is_validation":  isVal,
				"validation_url": valURL,
				"is_auth_error":  isAuth,
			})
			fmt.Println(string(out))
			return

		case "eval-crypto":
			var input struct {
				Action   string                    `json:"action"` // "encrypt" or "decrypt"
				Data     string                    `json:"data"`
				Password string                    `json:"password"`
				Salt     string                    `json:"salt"`
				Nonce    string                    `json:"nonce"`
				Bundle   *accounts.EncryptedBundle `json:"bundle"`
			}
			if err := readStdinJSON(&input); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(1)
			}
			if input.Action == "encrypt" {
				var salt, nonce []byte
				if input.Salt != "" {
					salt, _ = base64.StdEncoding.DecodeString(input.Salt)
				}
				if input.Nonce != "" {
					nonce, _ = base64.StdEncoding.DecodeString(input.Nonce)
				}
				b, err := accounts.EncryptBundle([]byte(input.Data), input.Password, salt, nonce)
				if err != nil {
					fmt.Fprintf(os.Stderr, "encrypt error: %v\n", err)
					os.Exit(1)
				}
				out, _ := json.Marshal(map[string]any{"bundle": b})
				fmt.Println(string(out))
				return
			} else if input.Action == "decrypt" {
				pt, err := accounts.DecryptBundle(input.Bundle, input.Password)
				if err != nil {
					out, _ := json.Marshal(map[string]any{"error": err.Error()})
					fmt.Println(string(out))
					return
				}
				out, _ := json.Marshal(map[string]any{"plaintext": string(pt)})
				fmt.Println(string(out))
				return
			}

		case "proxy-test-server":
			if len(os.Args) < 5 {
				fmt.Fprintf(os.Stderr, "usage: agy-pool proxy-test-server <port> <state-dir> <backend-url>\n")
				os.Exit(1)
			}
			port := os.Args[2]
			stateDir := os.Args[3]
			backendURL := os.Args[4]

			if err := config.ConfigureStateDir(stateDir); err != nil {
				fmt.Fprintf(os.Stderr, "config error: %v\n", err)
				os.Exit(1)
			}
			u, err := url.Parse(backendURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "invalid backend-url: %v\n", err)
				os.Exit(1)
			}
			proxy.BackendHostProvider = func() string { return u.Host }
			proxy.BackendURLProvider = func() string { return backendURL }
			proxy.TokenRefresher = func(acc *storage.Account) (string, error) {
				return "token-" + acc.ID, nil
			}

			server := &http.Server{
				Addr:    "127.0.0.1:" + port,
				Handler: proxy.NewHandler(),
			}
			fmt.Println("READY")
			if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fmt.Fprintf(os.Stderr, "server error: %v\n", err)
				os.Exit(1)
			}
			return
		}
	}

	fmt.Println("agy-pool-go: Go candidate implementation (Phase G2 Development)")
	fmt.Println("Usage: agy-pool <command> [args...]")
	fmt.Println("Available commands: version, eval-scheduler, eval-capacity, eval-account-target, eval-display-name, eval-jwt, eval-error-classifier, eval-crypto, proxy-test-server")
}
