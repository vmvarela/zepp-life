package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// tokenPromptMessage tells the user where the token comes from. It is written
// to the terminal only; the value itself is read with echo turned off.
const tokenPromptMessage = "Zepp app token (hidden input, see https://user.huami.com/privacy2/index.html): "

// Values accepted by --source.
const (
	srcCloudSession = "cloud-session"
	srcExportFile   = "export-file"
)

// setupReport is the stable JSON shape of `zepp-life setup`. It never contains
// the token.
type setupReport struct {
	Command    string `json:"command"`
	Status     string `json:"status"`  // ok | interaction_required | error
	Source     string `json:"source"`  // cloud-session | export-file
	Session    string `json:"session"` // reused | configured
	UserID     string `json:"user_id"`
	ConfigPath string `json:"config_path"`
	ExportPath string `json:"export_path"`
	Detail     string `json:"detail"`
}

// runSetup stores a usable Zepp cloud session, or configures an export folder.
//
// Preconditions: the configuration is readable. The source is chosen
// explicitly: without --source the cloud session keeps being the default, and
// an existing export_file configuration is only ever left when the user asks
// for it with --source cloud-session (or replaced by --source export-file).
// A complete saved session (token and user id) is reused as it is, with no
// prompt and no network access. Otherwise the token comes only from the hidden
// terminal prompt: there is no token flag and piped input is never accepted.
// The export branch validates the folder, writes only the configuration and
// never reads the keyring, prompts or performs any request.
//
// Postconditions on success: the token and user id are stored under the
// Python tool's credential names and config.json describes a cloud session,
// or config.json describes export_file with an absolute export_path — in both
// cases every other field is preserved, and no secret has been printed,
// logged or written to any output.
func runSetup(args []string, globalJSON bool, stdout, stderr io.Writer, env environment) int {
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	sourceFlag := flags.String("source", srcCloudSession, "what to configure: cloud-session or export-file")
	exportDirFlag := flags.String("export-dir", "", "folder with the exported activity data (with --source export-file)")
	userIDFlag := flags.String("user-id", "", "Zepp user id to store with the session")
	replaceToken := flags.Bool("replace-token", false, "enter a new token even if one is stored")
	jsonFlag := flags.Bool("json", false, "print JSON instead of human-readable text")
	if help, code := parseCommandFlags(flags, args, printSetupUsage, stdout, stderr); help || code != 0 {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s setup: unexpected argument %q\n\n", programName, flags.Arg(0))
		printSetupUsage(stderr)
		return exitUsage
	}
	source := strings.TrimSpace(*sourceFlag)
	if source != srcCloudSession && source != srcExportFile {
		fmt.Fprintf(stderr, "%s setup: unsupported --source %q; use %q or %q.\n\n",
			programName, source, srcCloudSession, srcExportFile)
		printSetupUsage(stderr)
		return exitUsage
	}
	exportDir := strings.TrimSpace(*exportDirFlag)
	switch {
	case source == srcExportFile && exportDir == "":
		fmt.Fprintf(stderr, "%s setup: --export-dir is required with --source %s.\n\n",
			programName, srcExportFile)
		printSetupUsage(stderr)
		return exitUsage
	case source == srcCloudSession && exportDir != "":
		fmt.Fprintf(stderr, "%s setup: --export-dir only applies with --source %s.\n\n",
			programName, srcExportFile)
		printSetupUsage(stderr)
		return exitUsage
	case source == srcExportFile && (strings.TrimSpace(*userIDFlag) != "" || *replaceToken):
		fmt.Fprintf(stderr, "%s setup: --user-id and --replace-token only apply with --source %s.\n\n",
			programName, srcCloudSession)
		printSetupUsage(stderr)
		return exitUsage
	}
	// Zepp user ids are numbers — the same rule discovery applies. Validate
	// the flag here so arbitrary input is never stored, reported as an
	// identity or allowed near a URL path.
	if requested := strings.TrimSpace(*userIDFlag); requested != "" && !allDigits(requested) {
		fmt.Fprintf(stderr, "%s setup: invalid --user-id %q; the Zepp user id is a number.\n\n",
			programName, requested)
		printSetupUsage(stderr)
		return exitUsage
	}
	jsonOut := globalJSON || *jsonFlag

	// sourceExplicit records that --source was passed at all: leaving an
	// export_file configuration for the cloud requires asking for it.
	sourceExplicit := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "source" {
			sourceExplicit = true
		}
	})

	report := setupReport{Command: "setup", Source: source, ConfigPath: env.configPath}
	emit := func(code int) int {
		emitReport(stdout, jsonOut, report, func(w io.Writer) { renderSetupHuman(w, report) })
		return code
	}
	fail := func(detail string) int {
		report.Status = "error"
		report.Detail = detail
		return emit(exitError)
	}

	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if state == configInvalid {
		return fail(err.Error())
	}

	if source == srcExportFile {
		return runSetupExport(cfg, exportDir, &report, emit, fail, env)
	}

	if cfg.mode == modeExportFile && !(sourceExplicit && source == srcCloudSession) {
		return fail("Your configuration uses export_file mode and was not changed. " +
			"Run `zepp-life setup --source export-file --export-dir DIR` to update the export folder, " +
			"or `zepp-life setup --source cloud-session` to switch to the Zepp cloud.")
	}

	token, hasToken, err := readCredential(env, keyringTokenAccount)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the stored Zepp token: %v", err))
	}
	storedUserID, _, err := readCredential(env, keyringUserIDAccount)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the stored user id: %v", err))
	}
	requestedUserID := strings.TrimSpace(*userIDFlag)
	userID := storedUserID
	if requestedUserID != "" {
		userID = requestedUserID
	}
	hasUserID := userID != ""
	needToken := *replaceToken || !hasToken

	// persistExplicitUserID stores --user-id when it actually changes the
	// stored value, at most once per run. The reuse path calls it before it
	// returns; the session path calls it after the token has been validated
	// (a rejected or unreadable token stores nothing at all) and before the
	// setup is declared complete.
	persistExplicitUserID := func() error {
		if requestedUserID == "" || requestedUserID == storedUserID {
			return nil
		}
		if err := writeCredential(env, keyringUserIDAccount, userID); err != nil {
			return fmt.Errorf("The user id could not be stored: %w", err)
		}
		return nil
	}

	// A complete saved session is reused untouched: no prompt, no request.
	if !needToken && hasUserID {
		if err := persistExplicitUserID(); err != nil {
			return fail(err.Error())
		}
		if err := updateConfigForCloud(env.configPath, env.databasePath); err != nil {
			return fail(err.Error())
		}
		report.Status = "ok"
		report.Session = "reused"
		report.UserID = userID
		report.Detail = "The saved session was reused."
		return emit(exitOK)
	}

	// The token can only come from the hidden prompt: never from a flag,
	// piped standard input or any output.
	if needToken {
		if jsonOut {
			report.Status = "interaction_required"
			report.Detail = "A Zepp app token is needed. Run `zepp-life setup` in a terminal " +
				"(without --json) to enter it; tokens are never read from flags or piped input."
			return emit(exitError)
		}
		prompted, err := env.promptSecret(tokenPromptMessage)
		switch {
		case errors.Is(err, errNoTTY):
			report.Status = "interaction_required"
			report.Detail = "A Zepp app token is needed and no interactive terminal is available. " +
				"Run `zepp-life setup` in a terminal to enter it."
			return emit(exitError)
		case err != nil:
			return fail(fmt.Sprintf("The token could not be read: %v", err))
		}
		token = strings.TrimSpace(prompted)
		if token == "" {
			return fail("No token was entered; nothing was stored.")
		}
	}

	// Anything new is validated with the cloud before it is stored, so a
	// rejected token never replaces a working one.
	api := newCloudAPI(env, token)
	if err := api.probe(); err != nil {
		return fail(err.Error())
	}
	if needToken {
		if err := writeCredential(env, keyringTokenAccount, token); err != nil {
			return fail(fmt.Sprintf("The token was accepted but could not be stored: %v", err))
		}
	}
	// An explicit --user-id is persisted here, exactly once: after the token
	// was accepted (or a complete token was reused) and before the session is
	// declared complete. Because the flag already made hasUserID true, this
	// also keeps an explicit id from triggering discovery below.
	if err := persistExplicitUserID(); err != nil {
		return fail(err.Error())
	}
	if !hasUserID {
		userID, err = api.discoverUserID(clockNow(env).In(cfg.location()))
		if err != nil {
			return fail(fmt.Sprintf("%v; run `zepp-life setup --user-id <id>` to store it yourself", err))
		}
		if err := writeCredential(env, keyringUserIDAccount, userID); err != nil {
			return fail(fmt.Sprintf("The user id was found but could not be stored: %v", err))
		}
	}
	if err := updateConfigForCloud(env.configPath, env.databasePath); err != nil {
		return fail(err.Error())
	}

	report.Status = "ok"
	if needToken {
		report.Session = "configured"
	} else {
		report.Session = "reused"
	}
	report.UserID = userID
	report.Detail = "The cloud session is ready."
	return emit(exitOK)
}

func renderSetupHuman(w io.Writer, r setupReport) {
	if r.Source == srcExportFile {
		fmt.Fprintln(w, "Zepp Life export setup")
		switch r.Status {
		case "ok":
			fmt.Fprintf(w, "  %-15s%s\n", "export dir:", orDash(r.ExportPath))
			fmt.Fprintf(w, "  %-15s%s\n", "config:", r.ConfigPath)
			fmt.Fprintln(w)
			fmt.Fprintln(w, "Ready. Next: zepp-life sync --type daily_activity")
		default:
			fmt.Fprintln(w, "  status:    error")
			fmt.Fprintf(w, "  %-15s%s\n", "detail:", r.Detail)
		}
		return
	}
	fmt.Fprintln(w, "Zepp Life cloud setup")
	switch r.Status {
	case "ok":
		fmt.Fprintf(w, "  %-15s%s\n", "session:", r.Session)
		fmt.Fprintf(w, "  %-15s%s\n", "user id:", orDash(r.UserID))
		fmt.Fprintf(w, "  %-15s%s\n", "config:", r.ConfigPath)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Ready. Next: zepp-life sync --type body_measurements")
	case "interaction_required":
		fmt.Fprintln(w, "  status:    interaction_required")
		fmt.Fprintf(w, "  %-15s%s\n", "detail:", r.Detail)
	default:
		fmt.Fprintln(w, "  status:    error")
		fmt.Fprintf(w, "  %-15s%s\n", "detail:", r.Detail)
	}
}

// runSetupExport points the configuration at an export folder.
//
// Preconditions: the configuration is readable, DIR exists, is a directory
// and holds at least one recognizable activity CSV (a header with a date and
// an activity metric column). Postconditions on success: config.json has
// mode=export_file and an absolute export_path, every other field is
// preserved, and neither the keyring, nor the prompt, nor the network has
// been touched. A folder without recognizable activity data is reported and
// the configuration is left unchanged.
func runSetupExport(cfg appConfig, exportDir string, report *setupReport, emit func(int) int, fail func(string) int, env environment) int {
	absDir, err := filepath.Abs(exportDir)
	if err != nil {
		return fail(fmt.Sprintf("The export folder path cannot be resolved: %v", err))
	}
	info, err := os.Stat(absDir)
	if err != nil {
		return fail(fmt.Sprintf("The export folder %s cannot be read: %v", absDir, err))
	}
	if !info.IsDir() {
		return fail(fmt.Sprintf("%s is not a folder.", absDir))
	}
	files, err := discoverActivityCSVs(absDir)
	if err != nil {
		return fail(err.Error())
	}
	// One budget for the whole validation: every header read is charged to
	// it, so an oversized or endless candidate stops the run here, before
	// any configuration is written.
	budget := &importBudget{}
	recognized := 0
	for _, path := range files {
		ok, err := recognizeActivityFile(path, budget)
		if err != nil {
			// Sync would fail on this candidate too; don't save a setup that
			// cannot be used as configured.
			return fail(err.Error())
		}
		if ok {
			recognized++
		}
	}
	if recognized == 0 {
		return fail(fmt.Sprintf(
			"%s does not look like an activity export: no CSV there has a date column and a steps, distance or calories column.",
			absDir))
	}

	alreadyConfigured := cfg.mode == modeExportFile && cfg.exportPath == absDir
	if !alreadyConfigured {
		if err := updateConfigForExport(env.configPath, env.databasePath, absDir); err != nil {
			return fail(err.Error())
		}
	}
	report.Status = "ok"
	report.ExportPath = absDir
	report.Session = "configured"
	report.Detail = fmt.Sprintf("%d activity CSV file(s) recognized.", recognized)
	return emit(exitOK)
}

func printSetupUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  zepp-life setup [--user-id ID] [--replace-token] [--json]
  zepp-life setup --source export-file --export-dir DIR [--json]

With no --source (or --source cloud-session) this stores your Zepp cloud
session (token and user id) so that `+"`zepp-life sync`"+` can download your
data. A saved session that is already complete is reused without asking
again. The token is read from a hidden terminal prompt; there is no token
flag and piped input is never accepted. An export_file configuration is only
left when --source cloud-session is asked for explicitly.

With --source export-file it validates that DIR exists and holds recognizable
activity CSVs, then stores its absolute path as export_path and switches the
mode to export_file, preserving every other configuration field. This branch
never reads the keyring, never prompts and never uses the network.

Flags:
  --source          cloud-session (default) or export-file
  --export-dir      Folder with the exported activity data (required with
                    --source export-file)
  --user-id         Zepp user id to store with the session
  --replace-token   Enter a new token even if one is stored
  --json            Print JSON instead of human-readable text
`)
}
