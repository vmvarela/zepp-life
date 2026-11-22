package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
)

// doctorReport is the stable JSON shape of `zepp-life doctor`.
type doctorReport struct {
	Command  string        `json:"command"`
	Status   string        `json:"status"` // ok | error
	Config   configCheck   `json:"config"`
	Export   exportCheck   `json:"export"`
	Database databaseCheck `json:"database"`
	Keyring  keyringCheck  `json:"keyring"`
}

type configCheck struct {
	Path         string `json:"path"`
	Status       string `json:"status"` // present | missing | invalid
	Mode         string `json:"mode"`
	Timezone     string `json:"timezone"`
	DatabasePath string `json:"database_path"`
	Detail       string `json:"detail"`
}

type databaseCheck struct {
	Path          string `json:"path"`
	Status        string `json:"status"` // ready | missing | unsupported_version | unreadable | skipped
	SchemaVersion int    `json:"schema_version"`
	Detail        string `json:"detail"`
}

type keyringCheck struct {
	Service   string `json:"service"`
	Status    string `json:"status"` // found | missing | error | skipped
	HasToken  bool   `json:"has_token"`
	HasUserID bool   `json:"has_user_id"`
	Detail    string `json:"detail"`
}

// exportCheck inspects the configured export folder read-only. It is skipped
// entirely unless the configuration uses export_file mode.
type exportCheck struct {
	Path   string `json:"path"`
	Status string `json:"status"` // ok | missing | not_found | no_activity | over_limit | skipped
	Detail string `json:"detail"`
}

func runDoctor(args []string, globalJSON bool, stdout, stderr io.Writer, env environment) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonFlag := flags.Bool("json", false, "print JSON instead of human-readable text")
	if help, code := parseCommandFlags(flags, args, printDoctorUsage, stdout, stderr); help || code != 0 {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s doctor: unexpected argument %q\n\n", programName, flags.Arg(0))
		printDoctorUsage(stderr)
		return exitUsage
	}
	jsonOut := globalJSON || *jsonFlag

	report := doctorReport{Command: "doctor"}
	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	report.Config = configCheck{Path: env.configPath, Status: string(state)}
	databasePath := env.databasePath
	exportMode := false

	switch state {
	case configMissing:
		report.Config.Detail = "No configuration file yet; this is normal on a clean install."
	case configPresent:
		databasePath = cfg.databasePath
		exportMode = cfg.mode == modeExportFile
		report.Config.Mode = cfg.mode
		report.Config.Timezone = cfg.timezone
		report.Config.DatabasePath = cfg.databasePath
		report.Config.Detail = "Configuration read."
	case configInvalid:
		report.Config.Detail = err.Error()
		report.Database = databaseCheck{
			Status: "skipped",
			Detail: "Not checked because the configuration cannot be read.",
		}
	}
	if state != configInvalid {
		report.Database = checkDatabase(databasePath)
	}
	report.Export = checkExport(cfg, state)
	// An export_file configuration has no cloud credentials to inspect: the
	// keyring is not read at all in that mode.
	if exportMode {
		report.Keyring = keyringCheck{
			Service: keyringService,
			Status:  "skipped",
			Detail:  "Not used in export_file mode; no cloud session is configured.",
		}
	} else {
		report.Keyring = checkKeyring(env)
	}

	report.Status = "ok"
	if doctorFailed(report) {
		report.Status = "error"
	}

	emitReport(stdout, jsonOut, report, func(w io.Writer) { renderDoctorHuman(w, report) })
	if report.Status == "error" {
		return exitError
	}
	return exitOK
}

func doctorFailed(r doctorReport) bool {
	if r.Config.Status == string(configInvalid) {
		return true
	}
	switch r.Database.Status {
	case "unsupported_version", "unreadable", "skipped":
		return true
	}
	// The export check is informational: a folder that has disappeared after
	// a successful import is normal — `daily` keeps working from the database.
	return r.Keyring.Status == "error"
}

// checkExport inspects the configured export folder without changing
// anything. It is skipped unless the configuration uses export_file mode.
func checkExport(cfg appConfig, state configState) exportCheck {
	if state != configPresent || cfg.mode != modeExportFile {
		return exportCheck{
			Status: "skipped",
			Detail: "Only checked in export_file mode.",
		}
	}
	check := exportCheck{Path: cfg.exportPath}
	if cfg.exportPath == "" {
		check.Status = "missing"
		check.Detail = "No export folder configured; run `zepp-life setup --source export-file --export-dir DIR`."
		return check
	}
	info, err := os.Stat(cfg.exportPath)
	if err != nil {
		check.Status = "not_found"
		check.Detail = fmt.Sprintf(
			"The folder cannot be read (%v). Already imported rows stay available with `zepp-life daily`.", err)
		return check
	}
	if !info.IsDir() {
		check.Status = "not_found"
		check.Detail = "The configured export_path is not a folder."
		return check
	}
	files, err := discoverActivityCSVs(cfg.exportPath)
	if err != nil {
		if errors.Is(err, errExportLimit) {
			check.Status = "over_limit"
		} else {
			check.Status = "not_found"
		}
		check.Detail = err.Error()
		return check
	}
	// The same budget the import uses keeps this inspection bounded too; a
	// folder that would exhaust an import is reported instead of read.
	budget := &importBudget{}
	for _, path := range files {
		ok, err := recognizeActivityFile(path, budget)
		if err != nil {
			if errors.Is(err, errExportLimit) {
				check.Status = "over_limit"
				check.Detail = err.Error()
				return check
			}
			continue
		}
		if ok {
			check.Status = "ok"
			check.Detail = "Readable, activity CSV data recognized."
			return check
		}
	}
	check.Status = "no_activity"
	check.Detail = "No CSV there has a date column and a steps, distance or calories column."
	return check
}

func checkDatabase(path string) databaseCheck {
	check := databaseCheck{Path: path}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		check.Status = "missing"
		check.Detail = "No local database yet; this is normal on a clean install."
		return check
	} else if err != nil {
		check.Status = "unreadable"
		check.Detail = fmt.Sprintf("Cannot access the database: %v", err)
		return check
	}

	db, err := openReadOnly(path)
	if err != nil {
		check.Status = "unreadable"
		check.Detail = fmt.Sprintf("Cannot open the database read-only: %v", err)
		return check
	}
	defer db.Close()

	version, err := schemaVersion(db)
	if err != nil {
		check.Status = "unreadable"
		check.Detail = fmt.Sprintf("Cannot read the schema version: %v", err)
		return check
	}
	check.SchemaVersion = version
	if version != expectedSchemaVersion {
		check.Status = "unsupported_version"
		if version == 0 {
			check.Detail = "The database has no schema yet (version 0). It was not modified."
		} else {
			check.Detail = fmt.Sprintf(
				"Schema version %d cannot be read by this build (it reads version %d). The database was not modified.",
				version, expectedSchemaVersion,
			)
		}
		return check
	}

	hasTable, err := bodyTableExists(db)
	if err != nil {
		check.Status = "unreadable"
		check.Detail = fmt.Sprintf("Cannot inspect the database: %v", err)
		return check
	}
	if !hasTable {
		check.Status = "unreadable"
		check.Detail = fmt.Sprintf("The %s table is missing from the database.", bodyTable)
		return check
	}
	check.Status = "ready"
	check.Detail = fmt.Sprintf("Readable, schema version %d.", version)
	return check
}

func checkKeyring(env environment) keyringCheck {
	check := keyringCheck{Service: keyringService}
	_, hasToken, tokenErr := readCredential(env, keyringTokenAccount)
	_, hasUserID, userIDErr := readCredential(env, keyringUserIDAccount)
	switch {
	case tokenErr != nil || userIDErr != nil:
		err := tokenErr
		if err == nil {
			err = userIDErr
		}
		check.Status = "error"
		check.Detail = fmt.Sprintf("Cannot read stored credentials: %v", err)
	case hasToken || hasUserID:
		check.Status = "found"
		check.HasToken = hasToken
		check.HasUserID = hasUserID
		check.Detail = "Stored credentials found."
	default:
		check.Status = "missing"
		check.Detail = "No stored credentials yet; this is normal on a clean install."
	}
	return check
}

func renderDoctorHuman(w io.Writer, r doctorReport) {
	fmt.Fprintln(w, "zepp-life doctor (read-only, no network access)")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Config")
	fmt.Fprintf(w, "  path:     %s\n", r.Config.Path)
	fmt.Fprintf(w, "  status:   %s\n", r.Config.Status)
	if r.Config.Status == string(configPresent) {
		fmt.Fprintf(w, "  mode:     %s\n", r.Config.Mode)
		fmt.Fprintf(w, "  timezone: %s\n", r.Config.Timezone)
		fmt.Fprintf(w, "  database: %s\n", r.Config.DatabasePath)
	}
	if r.Config.Detail != "" {
		fmt.Fprintf(w, "  note:     %s\n", r.Config.Detail)
	}
	fmt.Fprintln(w)

	if r.Export.Status != "skipped" {
		fmt.Fprintln(w, "Export folder")
		fmt.Fprintf(w, "  path:     %s\n", orDash(r.Export.Path))
		fmt.Fprintf(w, "  status:   %s\n", r.Export.Status)
		if r.Export.Detail != "" {
			fmt.Fprintf(w, "  note:     %s\n", r.Export.Detail)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "Database")
	fmt.Fprintf(w, "  path:     %s\n", r.Database.Path)
	fmt.Fprintf(w, "  status:   %s\n", r.Database.Status)
	if r.Database.Detail != "" {
		fmt.Fprintf(w, "  note:     %s\n", r.Database.Detail)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "Keyring")
	fmt.Fprintf(w, "  service:  %s\n", r.Keyring.Service)
	fmt.Fprintf(w, "  status:   %s\n", r.Keyring.Status)
	if r.Keyring.Status != "skipped" {
		fmt.Fprintf(w, "  token:    %s\n", storedText(r.Keyring.HasToken))
		fmt.Fprintf(w, "  user id:  %s\n", storedText(r.Keyring.HasUserID))
	}
	if r.Keyring.Detail != "" {
		fmt.Fprintf(w, "  note:     %s\n", r.Keyring.Detail)
	}
	fmt.Fprintln(w)

	if r.Status == "ok" {
		fmt.Fprintln(w, "Result: OK")
		return
	}
	fmt.Fprintln(w, "Result: problem found (see the notes above)")
}

func storedText(stored bool) string {
	if stored {
		return "stored"
	}
	return "not stored"
}
