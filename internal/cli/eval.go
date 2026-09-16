package cli

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/vlxlv/agy-pool-go/internal/accounts"
	"github.com/vlxlv/agy-pool-go/internal/auth"
	"github.com/vlxlv/agy-pool-go/internal/config"
	"github.com/vlxlv/agy-pool-go/internal/conversation"
	"github.com/vlxlv/agy-pool-go/internal/daemon"
	"github.com/vlxlv/agy-pool-go/internal/proxy"
	"github.com/vlxlv/agy-pool-go/internal/quota"
	"github.com/vlxlv/agy-pool-go/internal/scheduler"
	"github.com/vlxlv/agy-pool-go/internal/storage"
)

type evalSchedulerInput struct {
	Candidates []*storage.Account `json:"candidates"`
	Strategy   string             `json:"strategy"`
	Pool       *storage.Pool      `json:"pool"`
	Now        float64            `json:"now"`
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
		return err
	}
	return json.Unmarshal(data, v)
}

// HandleEval handles internal test commands required by differential parity test harnesses.
func HandleEval(cmd string, args []string) bool {
	switch cmd {
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
		return true

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
		return true

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
		return true

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
		return true

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
		return true

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
		return true

	case "eval-crypto":
		var input struct {
			Action   string                    `json:"action"`
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
			return true
		} else if input.Action == "decrypt" {
			pt, err := accounts.DecryptBundle(input.Bundle, input.Password)
			if err != nil {
				out, _ := json.Marshal(map[string]any{"error": err.Error()})
				fmt.Println(string(out))
				return true
			}
			out, _ := json.Marshal(map[string]any{"plaintext": string(pt)})
			fmt.Println(string(out))
			return true
		}
		return false

	case "proxy-test-server":
		if len(args) < 4 {
			fmt.Fprintf(os.Stderr, "usage: agy-pool proxy-test-server <port> <state-dir> <backend-url>\n")
			os.Exit(1)
		}
		port := args[1]
		stateDir := args[2]
		backendURL := args[3]

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
		return true

	case "eval-daemon":
		var input struct {
			Action      string `json:"action"`
			Bytes       int64  `json:"bytes"`
			Path        string `json:"path"`
			MaxBytes    int64  `json:"max_bytes"`
			BackupCount int    `json:"backup_count"`
			Force       bool   `json:"force"`
			Port        int    `json:"port"`
		}
		if err := readStdinJSON(&input); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		switch input.Action {
		case "format_size":
			formatted := daemon.FormatSize(input.Bytes)
			out, _ := json.Marshal(map[string]any{"formatted": formatted})
			fmt.Println(string(out))
			return true
		case "read_pid":
			info, err := daemon.ReadPIDFile(input.Path)
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			out, _ := json.Marshal(map[string]any{"info": info, "error": errMsg})
			fmt.Println(string(out))
			return true
		case "get_daemon_info":
			info := daemon.GetDaemonInfo(input.Path)
			out, _ := json.Marshal(map[string]any{"info": info})
			fmt.Println(string(out))
			return true
		case "rotate_log":
			rotated, err := daemon.RotateLogIfNeeded(input.Path, input.MaxBytes, input.BackupCount, input.Force)
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			out, _ := json.Marshal(map[string]any{"rotated": rotated, "error": errMsg})
			fmt.Println(string(out))
			return true
		case "clear_log":
			err := daemon.ClearLog(input.Path, input.BackupCount)
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			out, _ := json.Marshal(map[string]any{"error": errMsg})
			fmt.Println(string(out))
			return true
		case "stop_daemon":
			err := daemon.StopDaemon(input.Path, input.Port, 3*time.Second)
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			}
			out, _ := json.Marshal(map[string]any{"error": errMsg})
			fmt.Println(string(out))
			return true
		}
	case "eval-conversation":
		dir := ""
		if len(args) > 1 {
			dir = args[1]
		}
		cid, title, dirName, _ := conversation.FindLatestConversationForDir(dir)
		out, _ := json.Marshal(map[string]string{
			"cid":      cid,
			"title":    title,
			"dir_name": dirName,
		})
		fmt.Println(string(out))
		return true
	case "eval-resolve-continue":
		res := conversation.ResolveContinueArg(args[1:], io.Discard)
		out, _ := json.Marshal(map[string]any{
			"args": res,
		})
		fmt.Println(string(out))
		return true
	}
	return false
}
