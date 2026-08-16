package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/Fizzywood/deck-snapshot/internal/update"
)

func runUpdate(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 || (args[0] != "check" && args[0] != "install") {
		fmt.Fprintln(stderr, "Usage error: update requires check or install.")
		return ExitUsage
	}
	flags := flag.NewFlagSet("update "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: update accepts no positional arguments.")
		return ExitUsage
	}
	client := update.New(dependencies.HTTPClient)
	if args[0] == "install" {
		status, err := client.Install(nil, dependencies.Version)
		if err != nil {
			fmt.Fprintf(stderr, "Update failed safely: %v\n", err)
			return ExitRuntime
		}
		if *jsonOutput {
			if err := writeJSON(stdout, status); err != nil {
				fmt.Fprintln(stderr, "Unable to write update result.")
				return ExitRuntime
			}
			return ExitOK
		}
		fmt.Fprintf(stdout, "Installed: %s\nAvailable: %s\nUpdate installed: true\n", status.Installed, status.Available)
		return ExitOK
	}
	status, err := client.Check(nil, dependencies.Version)
	if err != nil {
		fmt.Fprintf(stderr, "Update check unavailable: %v\n", err)
		return ExitRuntime
	}
	if *jsonOutput {
		if err := writeJSON(stdout, status); err != nil {
			fmt.Fprintln(stderr, "Unable to write update result.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "Installed: %s\nAvailable: %s\nUp to date: %t\n", status.Installed, status.Available, status.UpToDate)
	return ExitOK
}
