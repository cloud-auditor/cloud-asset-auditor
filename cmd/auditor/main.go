package main

import (
	"os"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
