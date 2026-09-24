// Command afferent signs a developer in to authsrv and connects their coding
// agents' activity to brainsrv. It has its own command tree (see
// internal/afferent/cli) and does not reuse the upstream beacon root.
//
// Build: go build -o afferent ./cmd/afferent
package main

import (
	"os"

	"github.com/asymptote-labs/agent-beacon/cli/beacon/internal/afferent/cli"
)

func main() {
	os.Exit(cli.Execute())
}
