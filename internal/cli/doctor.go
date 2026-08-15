package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
)

type doctorResponse struct {
	Status            string `json:"status"`
	SteamDetected     bool   `json:"steam_detected"`
	DeckyDetected     bool   `json:"decky_detected"`
	CloudConfigured   bool   `json:"cloud_configured"`
	SteamAccounts     int    `json:"steam_accounts"`
	Plugins           int    `json:"plugins"`
	CSSThemes         int    `json:"css_themes"`
	Artwork           int    `json:"artwork"`
	CandidateFiles    int    `json:"candidate_files"`
	DiscoveryWarnings int    `json:"discovery_warnings"`
}

func runDoctor(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "explicit home root for read-only diagnostics")
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: doctor accepts no positional arguments.")
		return ExitUsage
	}
	paths, code := resolveSnapshotPaths(dependencies.Environment, *home, "", "", "", stderr)
	if code != ExitOK {
		return code
	}
	resourceLimits := limits.Default()
	result, err := discovery.Discover(context.Background(), discovery.Options{
		Paths: paths, AppVersion: dependencies.Version, DeviceID: "ds-doctor-read-only", DeviceName: "Diagnostics",
		Now: dependencies.Now().UTC(), Limits: resourceLimits,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Diagnostics failed safely: %v\n", err)
		return ExitRuntime
	}
	response := doctorResponse{
		Status: "ready", SteamDetected: isDirectory(paths.Steam), DeckyDetected: isDirectory(paths.Decky), CloudConfigured: isPrivateRegular(paths.CloudConfig),
		SteamAccounts: len(result.Manifest.Accounts), Plugins: len(result.Manifest.Plugins), CSSThemes: len(result.Manifest.CSSThemes), Artwork: len(result.Manifest.Artwork),
		CandidateFiles: len(result.Candidates), DiscoveryWarnings: len(result.Manifest.Warnings),
	}
	if !response.SteamDetected || !response.DeckyDetected || response.DiscoveryWarnings > 0 {
		response.Status = "attention_required"
	}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write diagnostics.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "Deck Snapshot diagnostics: %s\n", response.Status)
	fmt.Fprintf(stdout, "Steam detected: %t\nDecky detected: %t\nCloud configured: %t\n", response.SteamDetected, response.DeckyDetected, response.CloudConfigured)
	fmt.Fprintf(stdout, "Steam accounts: %d\nPlugins: %d\nCSS themes/profiles: %d\nCustom artwork: %d\nCandidate files: %d\nDiscovery warnings: %d\n",
		response.SteamAccounts, response.Plugins, response.CSSThemes, response.Artwork, response.CandidateFiles, response.DiscoveryWarnings)
	fmt.Fprintf(stdout, "Checked at: %s\n", dependencies.Now().UTC().Format(time.RFC3339))
	return ExitOK
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func isPrivateRegular(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0
}
