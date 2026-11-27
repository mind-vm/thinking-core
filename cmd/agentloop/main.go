// Command agentloop drives an agentloop.Loop from the terminal; see
// package cli for the command itself and its usage.
package main

import (
	"os"

	"github.com/mind-vm/thinking-core/cmd/agentloop/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], cli.Hooks{}))
}
