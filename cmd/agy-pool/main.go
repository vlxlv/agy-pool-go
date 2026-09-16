package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/vlxlv/agy-pool-go/internal/config"
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

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			fmt.Printf("agy-pool-go %s\n", config.Version)
			return

		case "eval-scheduler":
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to read stdin: %v\n", err)
				os.Exit(1)
			}
			var input evalSchedulerInput
			if err := json.Unmarshal(data, &input); err != nil {
				fmt.Fprintf(os.Stderr, "failed to parse input JSON: %v\n", err)
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
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				fmt.Fprintf(os.Stderr, "failed to read stdin: %v\n", err)
				os.Exit(1)
			}
			var input evalCapacityInput
			if err := json.Unmarshal(data, &input); err != nil {
				fmt.Fprintf(os.Stderr, "failed to parse input JSON: %v\n", err)
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
		}
	}

	fmt.Println("agy-pool-go: Go candidate implementation (Phase G1 Development)")
	fmt.Println("Usage: agy-pool <command> [args...]")
	fmt.Println("Available commands: version, eval-scheduler, eval-capacity")
}
