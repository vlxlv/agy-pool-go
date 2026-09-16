package main

import (
	"fmt"
	"os"

	"github.com/vlxlv/agy-pool-go/internal/config"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Printf("agy-pool-go %s\n", config.Version)
		return
	}

	fmt.Println("agy-pool-go: Go candidate implementation (Phase G1 Development)")
	fmt.Println("Usage: agy-pool <command> [args...]")
	fmt.Println("Available commands: version")
}
