package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	cloudcore "github.com/Fizzywood/deck-snapshot/internal/cloud"
	"github.com/Fizzywood/deck-snapshot/internal/limits"
	"github.com/Fizzywood/deck-snapshot/internal/platform"
)

const (
	cloudCryptRemote = "deck-snapshot-crypt"
	cloudBaseRemote  = "deck-snapshot-drive"
)

type cloudOptions struct {
	RecoveryFile         string
	Rclone               string
	JSON                 bool
	Legacy               bool
	Initialize           bool
	configPassword       string
	createConfigPassword bool
}

func runCloud(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage error: cloud requires recovery, connect, unlock, status, disconnect, list, inspect, upload, download, or trash.")
		return ExitUsage
	}
	switch args[0] {
	case "recovery":
		return runCloudRecovery(args[1:], stdout, stderr, dependencies)
	case "connect":
		return runCloudConnect(args[1:], stdout, stderr, dependencies)
	case "unlock":
		return runCloudUnlock(args[1:], stdout, stderr, dependencies)
	case "status":
		return runCloudStatus(args[1:], stdout, stderr, dependencies)
	case "disconnect":
		return runCloudDisconnect(args[1:], stdout, stderr, dependencies)
	case "list":
		return runCloudList(args[1:], stdout, stderr, dependencies)
	case "inspect":
		return runCloudInspect(args[1:], stdout, stderr, dependencies)
	case "upload":
		return runCloudUpload(args[1:], stdout, stderr, dependencies)
	case "download":
		return runCloudDownload(args[1:], stdout, stderr, dependencies)
	case "trash":
		return runCloudTrash(args[1:], stdout, stderr, dependencies)
	default:
		fmt.Fprintf(stderr, "Usage error: unknown cloud subcommand %q.\n", args[0])
		return ExitUsage
	}
}

func runCloudRecovery(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 || args[0] != "create" {
		fmt.Fprintln(stderr, "Usage error: cloud recovery requires create.")
		return ExitUsage
	}
	flags := flag.NewFlagSet("cloud recovery create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "absolute path for an optional recovery-key export")
	jsonOutput := flags.Bool("json", false, "write a JSON result")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 || *output == "" {
		fmt.Fprintln(stderr, "Usage error: cloud recovery create requires --output and no positional arguments.")
		return ExitUsage
	}
	absolute, err := filepath.Abs(*output)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the recovery export path.")
		return ExitUsage
	}
	paths, err := platform.Resolve(dependencies.Environment)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the private cloud recovery path.")
		return ExitRuntime
	}
	managedPath := filepath.Join(filepath.Dir(paths.CloudConfig), cloudcore.ManagedRecoveryFileName)
	material, loadErr := cloudcore.LoadRecovery(managedPath)
	if loadErr != nil {
		if _, managedErr := os.Lstat(managedPath); managedErr == nil {
			fmt.Fprintln(stderr, "The existing managed recovery key is missing or invalid; no new key was generated.")
			return ExitRuntime
		} else if !errors.Is(managedErr, os.ErrNotExist) {
			fmt.Fprintln(stderr, "The existing managed recovery key could not be inspected safely; no new key was generated.")
			return ExitRuntime
		}
		if _, statErr := os.Lstat(paths.CloudConfig); statErr == nil {
			fmt.Fprintln(stderr, "An existing cloud connection has no usable managed recovery key. Import and verify the existing recovery key before exporting it; no new key was generated.")
			return ExitRuntime
		} else if !errors.Is(statErr, os.ErrNotExist) {
			fmt.Fprintln(stderr, "The existing cloud configuration could not be inspected safely; no new key was generated.")
			return ExitRuntime
		}
		fmt.Fprintln(stderr, "No managed recovery key is available. Connect Google Drive or import an existing recovery key before exporting an Advanced fallback.")
		return ExitRuntime
	}
	if err := cloudcore.SaveRecovery(absolute, material); err != nil {
		fmt.Fprintf(stderr, "Unable to export recovery material: %v\n", err)
		return ExitRuntime
	}
	fingerprint, err := cloudcore.RecoveryFingerprint(material)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to fingerprint exported recovery material.")
		return ExitRuntime
	}
	response := struct {
		Path        string `json:"path"`
		Fingerprint string `json:"fingerprint"`
	}{Path: absolute, Fingerprint: fingerprint}
	if *jsonOutput {
		if err := writeJSON(stdout, response); err != nil {
			return ExitRuntime
		}
	} else {
		fmt.Fprintf(stdout, "Recovery key exported: %s\nFingerprint: %s\nThis is an optional Advanced fallback; normal Google Drive recovery is managed automatically.\n", absolute, fingerprint)
	}
	return ExitOK
}

func runCloudConnect(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud connect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: cloud connect accepts no positional arguments.")
		return ExitUsage
	}
	if options.Legacy {
		fmt.Fprintln(stderr, "Usage error: cloud connect cannot use --legacy.")
		return ExitUsage
	}
	if dependencies.GoogleClientID == "" || dependencies.GoogleClientCredential == "" {
		fmt.Fprintln(stderr, "Google Drive connection is unavailable because this development build has no complete embedded Desktop OAuth credential.")
		return ExitRuntime
	}
	options.createConfigPassword = true
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, false)
	if code != ExitOK {
		return code
	}
	var status cloudcore.Status
	var err error
	if options.Initialize {
		status, err = manager.ConnectGoogleWithInitialization(context.Background(), dependencies.GoogleClientID, dependencies.GoogleClientCredential, dependencies.Now())
	} else {
		status, err = manager.ConnectGoogle(context.Background(), dependencies.GoogleClientID, dependencies.GoogleClientCredential, dependencies.Now())
	}
	if err != nil {
		fmt.Fprintf(stderr, "Google Drive connection failed safely: %v\n", err)
		return ExitRuntime
	}
	message := "Google Drive connected with client-side protection.\n"
	if status.ConfigurationMessage != "" {
		message += "Note: " + status.ConfigurationMessage + "\n"
	}
	return writeCloudValue(stdout, stderr, options.JSON, status, message)
}

func runCloudUnlock(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud unlock", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: cloud unlock accepts no positional arguments.")
		return ExitUsage
	}
	if options.Legacy {
		fmt.Fprintln(stderr, "Usage error: cloud unlock cannot use --legacy.")
		return ExitUsage
	}
	password, err := readCloudConfigurationPassword(dependencies.Stdin)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to read the legacy cloud configuration password from standard input: %v\n", err)
		return ExitUsage
	}
	options.configPassword = password
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	status, err := manager.Check(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "Legacy cloud configuration unlock failed safely: %v\n", err)
		return ExitRuntime
	}
	paths, err := platform.Resolve(dependencies.Environment)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the local cloud configuration-key path.")
		return ExitRuntime
	}
	if err := cloudcore.SaveConfigPassword(cloudConfigPasswordPath(paths), password); err != nil {
		fmt.Fprintf(stderr, "Unable to persist the verified local cloud unlock: %v\n", err)
		return ExitRuntime
	}
	return writeCloudValue(stdout, stderr, options.JSON, status, "Legacy cloud configuration verified and unlocked for normal v0.1.1 use.\n")
}

func runCloudStatus(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: cloud status accepts no positional arguments.")
		return ExitUsage
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	status, err := manager.Check(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "Cloud status check failed: %v\n", err)
		return ExitRuntime
	}
	message := fmt.Sprintf("Cloud connected: %t\nClient-side protection: %t\nRecovery acknowledged: %t\nGoogle Drive snapshot scope: %s\nOAuth scopes: %s\nSnapshot folder: %s\nLegacy migration source: %t\n", status.Configured, status.Protected, status.RecoveryAcknowledged, status.Scope, status.OAuthScopes, status.Folder, status.Legacy)
	if status.ConfigurationMessage != "" {
		message += "Note: " + status.ConfigurationMessage + "\n"
	}
	return writeCloudValue(stdout, stderr, options.JSON, status, message)
}

func runCloudDisconnect(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud disconnect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	legacyPasswordStdin := flags.Bool("legacy-password-stdin", false, "read a locked v0.1.0 configuration password from standard input for local preservation")
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: cloud disconnect accepts no positional arguments.")
		return ExitUsage
	}
	if options.Legacy {
		fmt.Fprintln(stderr, "Usage error: preserved legacy connections are read-only and cannot be disconnected through this command.")
		return ExitUsage
	}
	if *legacyPasswordStdin {
		password, err := readCloudConfigurationPassword(dependencies.Stdin)
		if err != nil {
			fmt.Fprintf(stderr, "Unable to read the legacy cloud configuration password from standard input: %v\n", err)
			return ExitUsage
		}
		options.configPassword = password
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	status, err := manager.InspectConfiguration(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "Cloud disconnect preflight failed safely: %v\n", err)
		return ExitRuntime
	}
	paths, err := platform.Resolve(dependencies.Environment)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the local cloud paths.")
		return ExitRuntime
	}
	legacyPreserved := false
	if status.Legacy {
		var preserveErr error
		if *legacyPasswordStdin {
			preserveErr = cloudcore.PreserveLegacyConnectionWithPassword(paths.CloudConfig, options.configPassword, legacyCloudDirectory(paths))
		} else {
			preserveErr = cloudcore.PreserveLegacyConnection(paths.CloudConfig, cloudConfigPasswordPath(paths), legacyCloudDirectory(paths))
		}
		if preserveErr != nil {
			fmt.Fprintf(stderr, "Legacy cloud connection was not disconnected because its migration state could not be preserved: %v\n", preserveErr)
			return ExitRuntime
		}
		legacyPreserved = true
	} else if *legacyPasswordStdin {
		fmt.Fprintln(stderr, "Usage error: --legacy-password-stdin is accepted only for a verified v0.1.0 legacy connection.")
		return ExitUsage
	}
	if err := manager.Disconnect(context.Background()); err != nil {
		fmt.Fprintf(stderr, "Cloud disconnect failed safely: %v\n", err)
		return ExitRuntime
	}
	if cloudcore.RemoveConfigPassword(cloudConfigPasswordPath(paths)) != nil {
		fmt.Fprintln(stderr, "Google Drive was disconnected, but its local configuration key could not be removed safely.")
		return ExitRuntime
	}
	if options.JSON {
		return writeCloudValue(stdout, stderr, true, map[string]bool{"disconnected": true, "legacy_preserved": legacyPreserved}, "")
	}
	if legacyPreserved {
		fmt.Fprintln(stdout, "Legacy Google Drive connection preserved privately for migration and disconnected locally. Cloud and local snapshots were not removed.")
	} else {
		fmt.Fprintln(stdout, "Google Drive disconnected locally. Cloud snapshots, local snapshots, managed recovery, and exported fallback keys were not removed.")
	}
	return ExitOK
}

func runCloudList(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "Usage error: cloud list accepts no positional arguments.")
		return ExitUsage
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	items, err := manager.List(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "Unable to list protected cloud snapshots: %v\n", err)
		return ExitRuntime
	}
	if options.JSON {
		return writeCloudValue(stdout, stderr, true, items, "")
	}
	if len(items) == 0 {
		fmt.Fprintln(stdout, "No protected cloud snapshots were found.")
		return ExitOK
	}
	for _, item := range items {
		fmt.Fprintf(stdout, "%s  %d bytes  %s\n", item.Name, item.Size, item.ModTime)
	}
	return ExitOK
}

func runCloudInspect(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud inspect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	detailedOutput := flags.Bool("details", false, "include validated notice metadata in text output")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: cloud inspect requires exactly one protected cloud snapshot name.")
		return ExitUsage
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	value, item, err := manager.Inspect(context.Background(), flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "Unable to inspect protected cloud snapshot: %v\n", err)
		return ExitRuntime
	}
	return writeSnapshotDetails(stdout, stderr, options.JSON, *detailedOutput, snapshotDetailsFromManifest("", item.Name, item.Size, value))
}

func runCloudUpload(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud upload", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: cloud upload requires exactly one local snapshot path.")
		return ExitUsage
	}
	if options.Legacy {
		fmt.Fprintln(stderr, "Usage error: new uploads are not allowed through --legacy.")
		return ExitUsage
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	item, err := manager.Upload(context.Background(), flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "Protected cloud upload failed safely: %v\n", err)
		return ExitRuntime
	}
	return writeCloudValue(stdout, stderr, options.JSON, item, fmt.Sprintf("Protected snapshot uploaded and roundtrip-verified: %s\n", item.Name))
}

func runCloudDownload(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud download", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: cloud download requires exactly one protected cloud snapshot name.")
		return ExitUsage
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	path, err := manager.Download(context.Background(), flags.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "Protected cloud download failed safely: %v\n", err)
		return ExitRuntime
	}
	value := map[string]string{"path": path}
	return writeCloudValue(stdout, stderr, options.JSON, value, fmt.Sprintf("Protected snapshot downloaded and validated: %s\n", path))
}

func runCloudTrash(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("cloud trash", flag.ContinueOnError)
	flags.SetOutput(stderr)
	options := addCloudOptions(flags)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return ExitOK
		}
		return ExitUsage
	}
	if options.Legacy || flags.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage error: cloud trash requires exactly one non-legacy protected cloud snapshot name.")
		return ExitUsage
	}
	manager, code := buildCloudManager(context.Background(), *options, dependencies, stderr, true)
	if code != ExitOK {
		return code
	}
	if err := manager.Trash(context.Background(), flags.Arg(0)); err != nil {
		fmt.Fprintf(stderr, "Unable to move protected cloud snapshot to Google Drive Trash: %v\n", err)
		return ExitRuntime
	}
	fmt.Fprintln(stdout, "Protected snapshot moved to Google Drive Trash.")
	return ExitOK
}

func addCloudOptions(flags *flag.FlagSet) *cloudOptions {
	options := &cloudOptions{}
	flags.StringVar(&options.RecoveryFile, "recovery-file", "", "absolute path to an optional recovery-key fallback")
	flags.StringVar(&options.Rclone, "rclone", "", "absolute path to the pinned rclone executable")
	flags.BoolVar(&options.JSON, "json", false, "write a JSON result")
	flags.BoolVar(&options.Legacy, "legacy", false, "use the preserved read-only v0.1.0 app-folder connection")
	flags.BoolVar(&options.Initialize, "initialize", false, "after confirmation, create recovery data for an empty Google account")
	return options
}

func buildCloudManager(ctx context.Context, options cloudOptions, dependencies Dependencies, stderr io.Writer, requireRecovery bool) (cloudcore.Manager, int) {
	paths, err := platform.Resolve(dependencies.Environment)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to resolve application paths: %v\n", err)
		return cloudcore.Manager{}, ExitRuntime
	}
	stateDirectory := paths.State
	configPath := paths.CloudConfig
	passwordPath := cloudConfigPasswordPath(paths)
	if options.Legacy {
		legacyDirectory := legacyCloudDirectory(paths)
		stateDirectory = legacyDirectory
		configPath = filepath.Join(legacyDirectory, "rclone.conf")
		passwordPath = filepath.Join(legacyDirectory, "config-password")
	}
	managedRecoveryPath := filepath.Join(filepath.Dir(paths.CloudConfig), cloudcore.ManagedRecoveryFileName)
	var material *cloudcore.RecoveryMaterial
	manualRecovery := false
	if options.RecoveryFile != "" {
		recoveryPath, pathErr := filepath.Abs(options.RecoveryFile)
		if pathErr != nil {
			fmt.Fprintln(stderr, "Unable to resolve the recovery file path.")
			return cloudcore.Manager{}, ExitUsage
		}
		loaded, loadErr := cloudcore.LoadRecovery(recoveryPath)
		if loadErr != nil {
			fmt.Fprintf(stderr, "Unable to load recovery material: %v\n", loadErr)
			return cloudcore.Manager{}, ExitRuntime
		}
		material = &loaded
		manualRecovery = true
	} else if requireRecovery {
		loaded, loadErr := cloudcore.LoadRecovery(managedRecoveryPath)
		if loadErr != nil {
			if errors.Is(loadErr, os.ErrNotExist) || strings.Contains(loadErr.Error(), "missing or not a private regular file") {
				fmt.Fprintln(stderr, "Protected cloud recovery is not configured. Connect Google Drive first or import a recovery key from Advanced options.")
			} else {
				fmt.Fprintf(stderr, "Unable to load managed recovery material: %v\n", loadErr)
			}
			return cloudcore.Manager{}, ExitRuntime
		}
		material = &loaded
	}
	rclonePath := options.Rclone
	if rclonePath == "" {
		rclonePath = dependencies.RcloneBinary
	}
	if rclonePath == "" {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			fmt.Fprintln(stderr, "Unable to locate the installed rclone executable.")
			return cloudcore.Manager{}, ExitRuntime
		}
		rclonePath = filepath.Join(filepath.Dir(executable), "rclone")
	}
	rclonePath, err = filepath.Abs(rclonePath)
	if err != nil {
		fmt.Fprintln(stderr, "Unable to resolve the rclone executable path.")
		return cloudcore.Manager{}, ExitUsage
	}
	factory := dependencies.CloudRunnerFactory
	if factory == nil {
		factory = func(binary, config string) (cloudcore.Runner, error) {
			return cloudcore.NewRcloneRunner(binary, config)
		}
	}
	runner, err := factory(rclonePath, configPath)
	if err != nil {
		fmt.Fprintf(stderr, "Unable to initialize the cloud command boundary: %v\n", err)
		return cloudcore.Manager{}, ExitRuntime
	}
	var protected cloudcore.ProtectedRecovery
	if material != nil {
		protected, err = cloudcore.ProtectRecovery(ctx, runner, *material)
		if err != nil {
			fmt.Fprintf(stderr, "Unable to prepare recovery material: %v\n", err)
			return cloudcore.Manager{}, ExitRuntime
		}
	}
	configPassword := options.configPassword
	if configPassword == "" {
		if options.createConfigPassword {
			if _, configErr := os.Lstat(configPath); errors.Is(configErr, os.ErrNotExist) {
				configPassword, err = cloudcore.LoadOrCreateConfigPassword(passwordPath)
			} else {
				configPassword, err = cloudcore.LoadConfigPassword(passwordPath)
			}
		} else {
			configPassword, err = cloudcore.LoadConfigPassword(passwordPath)
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				fmt.Fprintln(stderr, "The existing cloud connection needs a one-time v0.1.1 unlock before it can be used.")
			} else {
				fmt.Fprintf(stderr, "Unable to load the private local cloud configuration key: %v\n", err)
			}
			return cloudcore.Manager{}, ExitRuntime
		}
	}
	manager := cloudcore.Manager{
		Runner: runner, SnapshotDirectory: paths.Snapshots, StateDirectory: stateDirectory, ConfigPath: configPath,
		RecoveryPath:   managedRecoveryPath,
		ConfigPassword: configPassword, CryptRemote: cloudCryptRemote, BaseRemote: cloudBaseRemote, BasePath: cloudcore.GoogleDriveBasePath, ExpectedBaseType: "drive",
		ProtectionFingerprint: protected.MaterialFingerprint, CryptPassword: protected.Password, CryptPassword2: protected.Password2,
		RecoveryMaterial: material, ManualRecovery: manualRecovery,
		AllowUnencryptedTest: dependencies.CloudAllowUnencryptedTest, Limits: limits.Default(),
	}
	return manager, ExitOK
}

func cloudConfigPasswordPath(paths platform.Paths) string {
	return filepath.Join(filepath.Dir(paths.CloudConfig), "config-password")
}

func legacyCloudDirectory(paths platform.Paths) string {
	return filepath.Join(filepath.Dir(paths.CloudConfig), cloudcore.LegacyConnectionDirectoryName)
}

func readCloudConfigurationPassword(input io.Reader) (string, error) {
	if input == nil {
		input = os.Stdin
	}
	scanner := bufio.NewScanner(io.LimitReader(input, 4097))
	scanner.Buffer(make([]byte, 1024), 2048)
	values := make([]string, 0, 1)
	for scanner.Scan() && len(values) < 1 {
		values = append(values, strings.TrimSuffix(scanner.Text(), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if len(values) != 1 {
		return "", errors.New("expected the cloud configuration password")
	}
	if len(values[0]) < 12 || len(values[0]) > 1024 || strings.ContainsAny(values[0], "\x00\r\n") {
		return "", errors.New("configuration password must contain 12 to 1024 characters without line breaks")
	}
	return values[0], nil
}

func writeCloudValue(stdout, stderr io.Writer, jsonOutput bool, value any, message string) int {
	if jsonOutput {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(value); err != nil {
			fmt.Fprintln(stderr, "Unable to write cloud result.")
			return ExitRuntime
		}
		return ExitOK
	}
	fmt.Fprint(stdout, message)
	return ExitOK
}
