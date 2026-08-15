package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/Fizzywood/deck-snapshot/internal/config"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

type settingsResponse struct {
	Path     string        `json:"path"`
	Settings config.Config `json:"settings"`
}

func runSettings(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage error: settings requires show or set.")
		return ExitUsage
	}
	switch args[0] {
	case "show":
		return runSettingsShow(args[1:], stdout, stderr, dependencies)
	case "set":
		return runSettingsSet(args[1:], stdout, stderr, dependencies)
	default:
		fmt.Fprintf(stderr, "Usage error: unknown settings subcommand %q.\n", args[0])
		return ExitUsage
	}
}

func runSettingsShow(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("settings show", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: settings show accepts no positional arguments.")
		return ExitUsage
	}
	paths, value, code := loadSettings(dependencies.Environment, stderr)
	if code != ExitOK {
		return code
	}
	response := settingsResponse{Path: config.Path(paths), Settings: value}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write settings.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "Automatic cloud upload: %t\n", value.AutoUpload)
	if value.RecoveryFile == "" {
		fmt.Fprintln(stdout, "Cloud recovery file: not selected")
	} else {
		fmt.Fprintf(stdout, "Cloud recovery file: %s\n", value.RecoveryFile)
	}
	return ExitOK
}

func runSettingsSet(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("settings set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	autoUpload := flags.String("auto-upload", "", "true or false")
	recoveryFile := flags.String("recovery-file", "", "absolute path to separate recovery material")
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: settings set accepts no positional arguments.")
		return ExitUsage
	}
	changed := map[string]bool{}
	flags.Visit(func(item *flag.Flag) { changed[item.Name] = true })
	if !changed["auto-upload"] && !changed["recovery-file"] {
		fmt.Fprintln(stderr, "Usage error: settings set requires at least one setting.")
		return ExitUsage
	}
	paths, value, code := loadSettings(dependencies.Environment, stderr)
	if code != ExitOK {
		return code
	}
	updates := []struct {
		name    string
		value   string
		target  *bool
		changed bool
	}{
		{name: "auto-upload", value: *autoUpload, target: &value.AutoUpload, changed: changed["auto-upload"]},
	}
	for _, update := range updates {
		if !update.changed {
			continue
		}
		parsed, err := strconv.ParseBool(update.value)
		if err != nil || (update.value != "true" && update.value != "false") {
			fmt.Fprintf(stderr, "Usage error: --%s must be exactly true or false.\n", update.name)
			return ExitUsage
		}
		*update.target = parsed
	}
	if changed["recovery-file"] {
		absolute, err := filepath.Abs(*recoveryFile)
		if err != nil || !filepath.IsAbs(absolute) || filepath.Clean(absolute) != absolute {
			fmt.Fprintln(stderr, "Usage error: --recovery-file must resolve to an absolute clean path.")
			return ExitUsage
		}
		value.RecoveryFile = absolute
	}
	if err := config.Save(config.Path(paths), value); err != nil {
		fmt.Fprintf(stderr, "Unable to save settings safely: %v\n", err)
		return ExitRuntime
	}
	response := settingsResponse{Path: config.Path(paths), Settings: value}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write settings.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintln(stdout, "Deck Snapshot settings saved.")
	return ExitOK
}

func loadSettings(environment platform.Environment, stderr io.Writer) (platform.Paths, config.Config, int) {
	paths, err := platform.Resolve(environment)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to resolve application paths: %v\n", err)
		return platform.Paths{}, config.Config{}, ExitRuntime
	}
	value, err := config.Load(config.Path(paths), config.Default(paths))
	if err != nil {
		fmt.Fprintf(stderr, "Unable to load settings safely: %v\n", err)
		return platform.Paths{}, config.Config{}, ExitRuntime
	}
	return paths, value, ExitOK
}
