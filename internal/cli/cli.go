// Package cli defines the stable command surface independently from the UI.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Fizzywood/deck-snapshot/internal/cloud"
	"github.com/Fizzywood/deck-snapshot/internal/deckyapi"
	"github.com/Fizzywood/deck-snapshot/internal/discovery"
	"github.com/Fizzywood/deck-snapshot/internal/identity"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/manifest"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
	"github.com/Fizzywood/deck-snapshot/internal/pluginstore"
	"github.com/Fizzywood/deck-snapshot/internal/restore"
	"github.com/Fizzywood/deck-snapshot/internal/snapshot"
)

const (
	ExitOK             = 0
	ExitUsage          = 2
	ExitNotImplemented = 3
	ExitRuntime        = 1
)

// Dependencies makes the command surface deterministic in tests.
type Dependencies struct {
	Environment               platform.Environment
	Version                   string
	Now                       func() time.Time
	Resolver                  pluginstore.Resolver
	HTTPClient                *http.Client
	Stdin                     io.Reader
	RcloneBinary              string
	CloudRunnerFactory        func(string, string) (cloud.Runner, error)
	CloudAllowUnencryptedTest bool
	DeckyInstaller            deckyapi.Installer
	GoogleClientID            string
	GoogleClientCredential    string
}

func Run(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if dependencies.Version == "" {
		dependencies.Version = "dev"
	}
	if dependencies.Now == nil {
		dependencies.Now = time.Now
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		writeHelp(stdout)
		return ExitOK
	}

	switch args[0] {
	case "version", "--version":
		fmt.Fprintf(stdout, "Deck Snapshot %s\n", dependencies.Version)
		return ExitOK
	case "paths":
		return runPaths(args[1:], stdout, stderr, dependencies.Environment)
	case "doctor":
		return runDoctor(args[1:], stdout, stderr, dependencies)
	case "snapshot":
		return runSnapshot(args[1:], stdout, stderr, dependencies)
	case "restore":
		return runRestore(args[1:], stdout, stderr, dependencies)
	case "cloud":
		return runCloud(args[1:], stdout, stderr, dependencies)
	case "settings":
		return runSettings(args[1:], stdout, stderr, dependencies)
	case "update":
		return runUpdate(args[1:], stdout, stderr, dependencies)
	}

	fmt.Fprintf(stderr, "Usage error: unknown command %q. Run 'deck-snapshot help'.\n", args[0])
	return ExitUsage
}

type restorePlanResponse struct {
	Path string       `json:"path"`
	Plan restore.Plan `json:"plan"`
}

func runRestore(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage error: restore requires plan, inspect, or run.")
		return ExitUsage
	}
	switch args[0] {
	case "plan":
		return runRestorePlan(args[1:], stdout, stderr, dependencies)
	case "inspect":
		return runRestoreInspect(args[1:], stdout, stderr)
	case "run":
		return runRestoreRun(args[1:], stdout, stderr, dependencies)
	default:
		fmt.Fprintf(stderr, "Usage error: unknown restore subcommand %q.\n", args[0])
		return ExitUsage
	}
}

func runRestorePlan(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("restore plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "explicit target home root")
	deckyHome := flags.String("decky-home", "", "explicit target Decky root")
	steamHome := flags.String("steam-home", "", "explicit target Steam root")
	stateDirectory := flags.String("state-dir", "", "explicit application state directory")
	planDirectory := flags.String("plan-dir", "", "restore plan output directory")
	jsonOutput := flags.Bool("json", false, "write the complete plan as JSON")
	detailedOutput := flags.Bool("details", false, "include every planned file and plugin action in text output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: restore plan requires exactly one snapshot path.")
		return ExitUsage
	}
	paths, code := resolveSnapshotPaths(dependencies.Environment, *home, *deckyHome, *steamHome, "", stderr)
	if code != ExitOK {
		return code
	}
	if *stateDirectory != "" {
		absolute, err := filepath.Abs(*stateDirectory)
		if err != nil {
			fmt.Fprintln(stderr, "Unable to resolve the state directory.")
			return ExitUsage
		}
		paths.State = absolute
		paths.Recovery = filepath.Join(absolute, "recovery")
	}
	if *planDirectory == "" {
		*planDirectory = filepath.Join(paths.State, "plans")
	} else {
		absolute, err := filepath.Abs(*planDirectory)
		if err != nil {
			fmt.Fprintln(stderr, "Unable to resolve the plan directory.")
			return ExitUsage
		}
		*planDirectory = absolute
	}
	snapshotPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the snapshot path.")
		return ExitUsage
	}
	resolver := dependencies.Resolver
	if resolver == nil {
		resolver = pluginstore.NewOfficial()
	}
	installer := dependencies.DeckyInstaller
	if installer == nil {
		installer = deckyapi.New()
	}
	plan, err := restore.BuildPlan(context.Background(), restore.PlanOptions{Paths: paths, SnapshotPath: snapshotPath, AppVersion: dependencies.Version, Now: dependencies.Now(), Limits: limits.Default(), Resolver: resolver, DeckyInstaller: installer})
	if err != nil {
		fmt.Fprintf(stderr, "Restore planning failed: %v\n", err)
		return ExitRuntime
	}
	planPath, err := restore.SavePlan(paths.Home, *planDirectory, plan)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to save the restore plan: %v\n", err)
		return ExitRuntime
	}
	response := restorePlanResponse{Path: planPath, Plan: plan}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write the restore plan.")
			return ExitRuntime
		}
		return ExitOK
	}
	writeRestorePlanText(stdout, planPath, plan, "created")
	if *detailedOutput {
		writeRestorePlanActions(stdout, plan)
	}
	return ExitOK
}

func runRestoreInspect(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("restore inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write the complete plan as JSON")
	detailedOutput := flags.Bool("details", false, "include every planned file and plugin action in text output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: restore inspect requires exactly one saved plan path.")
		return ExitUsage
	}
	planPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the restore plan path.")
		return ExitUsage
	}
	plan, err := restore.LoadPlan(planPath)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to load the restore plan safely: %v\n", err)
		return ExitRuntime
	}
	if *jsonOutput {
		if err := writeJSON(stdout, restorePlanResponse{Path: planPath, Plan: plan}); err != nil {
			fmt.Fprintln(stderr, "Unable to write the restore plan.")
			return ExitRuntime
		}
		return ExitOK
	}
	writeRestorePlanText(stdout, planPath, plan, "loaded")
	if *detailedOutput {
		writeRestorePlanActions(stdout, plan)
	}
	return ExitOK
}

func writeRestorePlanText(stdout io.Writer, planPath string, plan restore.Plan, disposition string) {
	fmt.Fprintf(stdout, "Restore plan %s without target writes: %s\n", disposition, planPath)
	fmt.Fprintf(stdout, "Plan ID: %s\nApproval hash: %s\nActions: %d\nPlugins: %d\nBlocking: %t\n", plan.PlanID, plan.ApprovalHash, len(plan.Actions), len(plan.PluginActions), plan.Blocking)
	fmt.Fprintf(stdout, "Required free space: %d bytes\n", plan.RequiredFreeBytes)
	for _, warning := range plan.Warnings {
		fmt.Fprintf(stdout, "Warning: %s\n", warning)
	}
}

func writeRestorePlanActions(stdout io.Writer, plan restore.Plan) {
	for _, action := range plan.Actions {
		fmt.Fprintf(stdout, "File action: %s | %s | %s -> %s\n", action.Operation, action.Component, action.LogicalPath, action.TargetPath)
	}
	for _, action := range plan.PluginActions {
		fmt.Fprintf(stdout, "Plugin action: %s | %s -> %s", action.Operation, action.Directory, action.TargetPath)
		if action.Reason != "" {
			fmt.Fprintf(stdout, " | %s", action.Reason)
		}
		fmt.Fprintln(stdout)
	}
}

func runRestoreRun(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("restore run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	approvedPlanID := flags.String("approve", "", "exact restore plan ID shown by restore plan")
	approvedHash := flags.String("approval-hash", "", "exact full approval hash shown by restore plan")
	jsonOutput := flags.Bool("json", false, "write the restore report as JSON")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 || *approvedPlanID == "" || *approvedHash == "" {
		fmt.Fprintln(stderr, "Usage error: restore run requires a plan path, --approve, and --approval-hash.")
		return ExitUsage
	}
	planPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the restore plan path.")
		return ExitUsage
	}
	plan, err := restore.LoadPlan(planPath)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to load the restore plan: %v\n", err)
		return ExitRuntime
	}
	report, runErr := restore.Run(context.Background(), restore.RunOptions{
		Plan: plan, ApprovedPlanID: *approvedPlanID, ApprovedHash: *approvedHash, Limits: limits.Default(),
		WorkDirectory: filepath.Join(plan.Target.State, "work"), RecoveryDirectory: filepath.Join(plan.Target.State, "recovery"), ReportDirectory: filepath.Join(plan.Target.State, "reports"),
		Now: dependencies.Now, HTTPClient: dependencies.HTTPClient, DeckyInstaller: deckyInstaller(dependencies),
	})
	if *jsonOutput {
		if err := writeJSON(stdout, report); err != nil {
			fmt.Fprintln(stderr, "Unable to write the restore report.")
			return ExitRuntime
		}
	} else {
		fmt.Fprintf(stdout, "Restore status: %s\nRecovery snapshot: %s\nReport: %s\n", report.Status, report.RecoverySnapshotPath, report.ReportPath)
	}
	if runErr != nil {
		fmt.Fprintf(stderr, "Restore failed safely: %v\n", runErr)
		return ExitRuntime
	}
	return ExitOK
}

func deckyInstaller(dependencies Dependencies) deckyapi.Installer {
	if dependencies.DeckyInstaller != nil {
		return dependencies.DeckyInstaller
	}
	return deckyapi.New()
}

type createResponse struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SnapshotID string `json:"snapshot_id"`
	CreatedUTC string `json:"created_utc"`
	Files      int    `json:"files"`
	Warnings   int    `json:"warnings"`
	Valid      bool   `json:"valid"`
}

func runSnapshot(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage error: snapshot requires create, list, inspect, validate, or delete.")
		return ExitUsage
	}
	switch args[0] {
	case "create":
		return runSnapshotCreate(args[1:], stdout, stderr, dependencies)
	case "list":
		return runSnapshotList(args[1:], stdout, stderr, dependencies)
	case "inspect":
		return runSnapshotInspect(args[1:], stdout, stderr)
	case "validate":
		return runSnapshotValidate(args[1:], stdout, stderr)
	case "delete":
		return runSnapshotDelete(args[1:], stdout, stderr, dependencies)
	default:
		fmt.Fprintf(stderr, "Usage error: unknown snapshot subcommand %q.\n", args[0])
		return ExitUsage
	}
}

type snapshotDetailsResponse struct {
	Path           string                 `json:"path"`
	Name           string                 `json:"name"`
	Size           int64                  `json:"size"`
	Valid          bool                   `json:"valid"`
	SnapshotID     string                 `json:"snapshot_id"`
	CreatedUTC     string                 `json:"created_utc"`
	AppVersion     string                 `json:"app_version"`
	DeviceName     string                 `json:"device_name,omitempty"`
	SteamOSVersion string                 `json:"steamos_version,omitempty"`
	DeckyVersion   string                 `json:"decky_version,omitempty"`
	Files          int                    `json:"files"`
	FileBytes      int64                  `json:"file_bytes"`
	Plugins        int                    `json:"plugins"`
	CSSThemes      int                    `json:"css_themes"`
	Artwork        int                    `json:"artwork"`
	Exclusions     int                    `json:"exclusions"`
	Warnings       []manifest.Warning     `json:"warnings"`
	Components     map[string]int         `json:"components"`
	Compatibility  manifest.Compatibility `json:"compatibility"`
}

func runSnapshotInspect(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("snapshot inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	detailedOutput := flags.Bool("details", false, "include validated notice metadata in text output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: snapshot inspect requires exactly one snapshot path.")
		return ExitUsage
	}
	snapshotPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the snapshot path.")
		return ExitUsage
	}
	value, err := snapshot.Validate(snapshotPath, limits.Default())
	if err != nil {
		fmt.Fprintf(stderr, "Snapshot inspection failed validation: %v\n", err)
		return ExitRuntime
	}
	info, err := os.Stat(snapshotPath)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to inspect the validated snapshot.")
		return ExitRuntime
	}
	return writeSnapshotDetails(stdout, stderr, *jsonOutput, *detailedOutput, snapshotDetailsFromManifest(snapshotPath, filepath.Base(snapshotPath), info.Size(), value))
}

func snapshotDetailsFromManifest(path, name string, size int64, value manifest.Manifest) snapshotDetailsResponse {
	response := snapshotDetailsResponse{Path: path, Name: name, Size: size, Valid: true, SnapshotID: value.SnapshotID, CreatedUTC: value.CreatedUTC, AppVersion: value.AppVersion, DeviceName: value.Device.Name, SteamOSVersion: value.Detected.SteamOSVersion, DeckyVersion: value.Detected.DeckyVersion, Files: len(value.Files), Plugins: len(value.Plugins), CSSThemes: len(value.CSSThemes), Artwork: len(value.Artwork), Exclusions: len(value.Exclusions), Warnings: value.Warnings, Components: make(map[string]int), Compatibility: value.Compatibility}
	for _, file := range value.Files {
		response.FileBytes += file.Size
		response.Components[file.Component]++
	}
	return response
}

func writeSnapshotDetails(stdout, stderr io.Writer, jsonOutput, detailedOutput bool, response snapshotDetailsResponse) int {
	if jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write snapshot details.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "Created: %s\nSize: %d bytes\nFiles: %d (%d bytes)\nPlugins: %d\nCSS themes/profiles: %d\nArtwork: %d\nWarnings: %d\n", response.CreatedUTC, response.Size, response.Files, response.FileBytes, response.Plugins, response.CSSThemes, response.Artwork, len(response.Warnings))
	if detailedOutput {
		for _, warning := range response.Warnings {
			fmt.Fprintf(stdout, "Notice: %s\t%s\t%s\n", warning.Code, warning.Component, warning.Message)
		}
	}
	return ExitOK
}

func runSnapshotDelete(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("snapshot delete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "explicit home root")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: snapshot delete requires exactly one snapshot filename.")
		return ExitUsage
	}
	paths, code := resolveSnapshotPaths(dependencies.Environment, *home, "", "", "", stderr)
	if code != ExitOK {
		return code
	}
	if err := snapshot.DeleteValidated(context.Background(), paths.Snapshots, flags.Arg(0), limits.Default()); err != nil {
		fmt.Fprintf(stderr, "Snapshot deletion failed safely: %v\n", err)
		return ExitRuntime
	}
	fmt.Fprintln(stdout, "Local snapshot deleted.")
	return ExitOK
}

func runSnapshotCreate(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("snapshot create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "explicit home root for discovery")
	deckyHome := flags.String("decky-home", "", "explicit Decky home root")
	steamHome := flags.String("steam-home", "", "explicit Steam root")
	outputDirectory := flags.String("output-dir", "", "snapshot output directory")
	deviceID := flags.String("device-id", "", "existing non-sensitive device ID")
	deviceName := flags.String("device-name", "", "optional device display name")
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: snapshot create does not accept positional arguments.")
		return ExitUsage
	}

	paths, code := resolveSnapshotPaths(dependencies.Environment, *home, *deckyHome, *steamHome, *outputDirectory, stderr)
	if code != ExitOK {
		return code
	}
	if *deviceID == "" {
		var err error
		*deviceID, err = identity.LoadOrCreate(paths.State)
		if err != nil {
			fmt.Fprintf(stderr, "Unable to load the device identity: %v\n", err)
			return ExitRuntime
		}
	}
	resourceLimits := limits.Default()
	result, err := discovery.Discover(context.Background(), discovery.Options{
		Paths:      paths,
		AppVersion: dependencies.Version,
		DeviceID:   *deviceID,
		DeviceName: *deviceName,
		Now:        dependencies.Now(),
		Limits:     resourceLimits,
	})
	if err != nil {
		fmt.Fprintf(stderr, "Snapshot discovery failed: %v\n", err)
		return ExitRuntime
	}
	created, err := snapshot.Create(context.Background(), paths.Snapshots, result, resourceLimits)
	if err != nil {
		fmt.Fprintf(stderr, "Snapshot creation failed: %v\n", err)
		return ExitRuntime
	}
	response := createResponse{
		Path:       created.Path,
		Size:       created.Size,
		SnapshotID: created.Manifest.SnapshotID,
		CreatedUTC: created.Manifest.CreatedUTC,
		Files:      len(created.Manifest.Files),
		Warnings:   len(created.Manifest.Warnings),
		Valid:      true,
	}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write the snapshot result.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "Snapshot created and validated: %s\n", response.Path)
	fmt.Fprintf(stdout, "Snapshot ID: %s\nFiles: %d\nWarnings: %d\n", response.SnapshotID, response.Files, response.Warnings)
	return ExitOK
}

func runSnapshotList(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("snapshot list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home := flags.String("home", "", "explicit home root")
	outputDirectory := flags.String("output-dir", "", "snapshot directory")
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: snapshot list does not accept positional arguments.")
		return ExitUsage
	}
	paths, code := resolveSnapshotPaths(dependencies.Environment, *home, "", "", *outputDirectory, stderr)
	if code != ExitOK {
		return code
	}
	entries, err := snapshot.List(paths.Snapshots, limits.Default())
	if err != nil {
		fmt.Fprintf(stderr, "Unable to list snapshots: %v\n", err)
		return ExitRuntime
	}
	if *jsonOutput {
		if err := writeJSON(stdout, entries); err != nil {
			fmt.Fprintln(stderr, "Unable to write the snapshot list.")
			return ExitRuntime
		}
		return ExitOK
	}
	if len(entries) == 0 {
		fmt.Fprintln(stdout, "No local snapshots were found.")
		return ExitOK
	}
	for _, entry := range entries {
		status := "invalid"
		if entry.Valid {
			status = "valid"
		}
		fmt.Fprintf(stdout, "%s  %s  %d bytes  %s\n", entry.CreatedUTC, entry.SnapshotID, entry.Size, status)
	}
	return ExitOK
}

func runSnapshotValidate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("snapshot validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: snapshot validate requires exactly one snapshot path.")
		return ExitUsage
	}
	snapshotPath, err := filepath.Abs(flags.Arg(0))
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the snapshot path.")
		return ExitRuntime
	}
	value, err := snapshot.Validate(snapshotPath, limits.Default())
	if err != nil {
		fmt.Fprintf(stderr, "Snapshot validation failed: %v\n", err)
		return ExitRuntime
	}
	info, err := os.Stat(snapshotPath)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to inspect the validated snapshot.")
		return ExitRuntime
	}
	response := createResponse{Path: snapshotPath, Size: info.Size(), SnapshotID: value.SnapshotID, CreatedUTC: value.CreatedUTC, Files: len(value.Files), Warnings: len(value.Warnings), Valid: true}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			fmt.Fprintln(stderr, "Unable to write the validation result.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprintf(stdout, "Snapshot is valid: %s\nSnapshot ID: %s\nFiles: %d\nWarnings: %d\n", snapshotPath, value.SnapshotID, len(value.Files), len(value.Warnings))
	return ExitOK
}

func resolveSnapshotPaths(environment platform.Environment, home, deckyHome, steamHome, outputDirectory string, stderr io.Writer) (platform.Paths, int) {
	if environment == nil {
		environment = platform.OSEnvironment{}
	}
	if home != "" {
		absolute, err := filepath.Abs(home)
		if err != nil {
			fmt.Fprintln(stderr, "Unable to resolve the explicit home root.")
			return platform.Paths{}, ExitUsage
		}
		environment = isolatedHomeEnvironment{home: absolute}
	}
	paths, err := platform.Resolve(environment)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to resolve application paths: %v\n", err)
		return platform.Paths{}, ExitRuntime
	}
	overrides := []struct {
		value  string
		target *string
	}{
		{value: deckyHome, target: &paths.Decky},
		{value: steamHome, target: &paths.Steam},
		{value: outputDirectory, target: &paths.Snapshots},
	}
	for _, override := range overrides {
		if override.value == "" {
			continue
		}
		absolute, err := filepath.Abs(override.value)
		if err != nil {
			fmt.Fprintf(stderr, "Unable to resolve path %q.\n", override.value)
			return platform.Paths{}, ExitUsage
		}
		*override.target = absolute
	}
	return paths, ExitOK
}

type isolatedHomeEnvironment struct{ home string }

func (environment isolatedHomeEnvironment) LookupEnv(string) (string, bool) { return "", false }
func (environment isolatedHomeEnvironment) UserHomeDir() (string, error) {
	return environment.home, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func runPaths(args []string, stdout, stderr io.Writer, environment platform.Environment) int {
	jsonOutput := false
	if len(args) > 1 || (len(args) == 1 && args[0] != "--json") {
		fmt.Fprintln(stderr, "Usage error: paths accepts only --json.")
		return ExitUsage
	}
	if len(args) == 1 {
		jsonOutput = true
	}

	paths, err := platform.Resolve(environment)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to resolve application paths: %v\n", err)
		return ExitRuntime
	}
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(paths); err != nil {
			fmt.Fprintln(stderr, "Unable to write path report.")
			return ExitRuntime
		}
		return ExitOK
	}

	entries := map[string]string{
		"Cache":         paths.Cache,
		"Cloud config":  paths.CloudConfig,
		"Configuration": paths.Config,
		"Decky":         paths.Decky,
		"Recovery":      paths.Recovery,
		"Snapshots":     paths.Snapshots,
		"Steam":         paths.Steam,
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(stdout, "%-15s %s\n", key+":", entries[key])
	}
	return ExitOK
}

func writeHelp(output io.Writer) {
	help := `Deck Snapshot preserves supported Steam Deck customization state.

Usage:
  deck-snapshot help
  deck-snapshot version
  deck-snapshot paths [--json]
  deck-snapshot doctor [--json] [--home <path>]
  deck-snapshot snapshot create [options]
  deck-snapshot snapshot list [options]
  deck-snapshot snapshot inspect [--json] <path>
  deck-snapshot snapshot validate [--json] <path>
  deck-snapshot snapshot delete [--home <path>] <snapshot-filename>
  deck-snapshot restore plan [options] [--details] <snapshot>
  deck-snapshot restore inspect [--json] [--details] <plan>
  deck-snapshot restore run --approve <plan-id> --approval-hash <hash> <plan>
  deck-snapshot cloud recovery create --output <path>
  deck-snapshot cloud connect [--recovery-file <path>] [--initialize]
  deck-snapshot cloud unlock [--recovery-file <path>]
  deck-snapshot cloud status|list [cloud options] [--legacy]
  deck-snapshot cloud inspect|trash [cloud options] <cloud-name>
  deck-snapshot cloud disconnect [--legacy-password-stdin] [cloud options]
  deck-snapshot cloud upload [cloud options] <snapshot>
  deck-snapshot cloud download [cloud options] [--legacy] <cloud-name>
  deck-snapshot settings show [--json]
  deck-snapshot settings set [--auto-upload true|false] [--recovery-file <path>]
  deck-snapshot update check|install [--json]

Local discovery, immutable validated snapshots, dry-run restore planning, verified
recovery, plugin package validation, transactional restore and protected cloud
transfer are implemented. New connections use a generated private local
configuration key and the app's embedded Google Desktop OAuth credential. A
v0.1.0 configuration needs its existing password once through
standard input for explicit unlock or locally verified preservation before
disconnect; it is never an argument.
`
	fmt.Fprint(output, strings.TrimLeft(help, "\n"))
}
