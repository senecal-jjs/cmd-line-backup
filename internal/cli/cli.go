// Package cli implements gobackup's command-line interface: subcommand dispatch,
// shared flag/environment/config resolution, and the command implementations.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
)

// ErrUsage signals that usage information was already written to the output and
// no further error message should be printed.
var ErrUsage = errors.New("usage")

// command is a single gobackup subcommand.
type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string, out, errOut io.Writer) error
}

func commands() []command {
	return []command{
		{"init", "create a new backup repository", runInit},
		{"backup", "create a snapshot of one or more paths", runBackup},
		{"list", "list snapshots, or the files within a snapshot", runList},
		{"restore", "restore files from a snapshot", runRestore},
		{"verify", "check repository integrity", runVerify},
	}
}

var version = "dev"

// Run dispatches to a subcommand.
func Run(args []string, out, errOut io.Writer) error {
	if len(args) == 0 {
		usage(errOut)
		return ErrUsage
	}

	name := args[0]
	switch name {
	case "-h", "--help", "help":
		usage(out)
		return nil
	case "-version", "--version", "version":
		fmt.Fprintf(out, "gobackup %s\n", version)
		return nil
	}

	for _, c := range commands() {
		if c.name == name {
			return c.run(context.Background(), args[1:], out, errOut)
		}
	}

	fmt.Fprintf(errOut, "gobackup: unknown command %q\n\n", name)
	usage(errOut)
	return ErrUsage
}

func usage(w io.Writer) {
	fmt.Fprint(w, `gobackup - incremental, encrypted, deduplicating backups to Amazon S3

Usage:
  gobackup <command> [flags] [arguments]

Commands:
`)
	cmds := commands()
	sort.Slice(cmds, func(i, j int) bool { return cmds[i].name < cmds[j].name })
	for _, c := range cmds {
		fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	fmt.Fprint(w, `
Run "gobackup <command> -h" for command-specific flags.

Environment:
  GOBACKUP_REPO        repository location, e.g. s3://bucket/prefix
  GOBACKUP_KEYFILE     path to the encryption keyfile
  GOBACKUP_PASSPHRASE  passphrase protecting the keyfile
`)
}
