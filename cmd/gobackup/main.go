// Command gobackup is an incremental, encrypted, deduplicating backup utility.
package main

import (
	"fmt"
	"os"

	"github.com/senecal-jjs/cmd-line-backup/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if err != cli.ErrUsage {
			fmt.Fprintf(os.Stderr, "gobackup: %v\n", err)
		}
		os.Exit(1)
	}
}
