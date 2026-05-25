package main

import (
	"os"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/cli"

	// Provider registrations. Each blank import fires a package init() that
	// calls core.Register(name, factory). Add new providers here.
	_ "github.com/cloud-auditor/cloud-asset-auditor/internal/providers/cloudflare"
)

func main() {
	os.Exit(cli.Execute())
}
