package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
)

// dailyReport is the stable JSON shape of `zepp-life daily`.
type dailyReport struct {
	Command string        `json:"command"`
	Status  string        `json:"status"` // ok | no_data | error
	Date    string        `json:"date"`
	UserID  string        `json:"user_id"`
	Records []dailyRecord `json:"records"`
	Detail  string        `json:"detail"`
}

// dailyRecord is one device's row for the requested date. Devices are kept
// apart on purpose: their windows can overlap, so they are never summed.
type dailyRecord struct {
	DeviceID   *string `json:"device_id"`
	DeviceKey  string  `json:"device_key"`
	Steps      int64   `json:"steps"`
	DistanceM  float64 `json:"distance_m"`
	ActiveKcal float64 `json:"active_kcal"`
}

// activitySourceFilter is the source_type `daily` resolves profiles and rows
// against for the configured mode. In cloud_session mode only cloud-origin
// rows count, so rows imported from an export folder can neither make the
// profile ambiguous nor be reported as cloud data. Every other mode keeps
// reading the table as it always did.
func activitySourceFilter(mode string) string {
	if mode == modeCloudSession {
		return sourceCloudSession
	}
	return ""
}

// runDaily shows the daily activity stored for one date.
//
// Preconditions (P): --date is a canonical YYYY-MM-DD date, the configuration
// is readable, and the database — when present — holds the version 6 schema
// with the daily_activity table. The profile is derived deterministically
// from the configured absolute export_path only in export_file mode (in any
// other mode that field may be stale after a source switch and is ignored);
// otherwise only the single profile stored in the table may be used, and
// several profiles are an error, never a guess. In cloud_session mode that
// search, and the row query below, are restricted to cloud-origin rows.
//
// Postconditions (Q): the report holds all and only the rows of that profile
// on that date — of the configured source when there is one — in a stable
// order (device_key, then id), and nothing else is read or written: the
// database is opened read-only, and neither the keyring nor the network is
// touched — cloud and export rows are both read from the local cache. No rows
// is a normal result (exit 0); an unusable database or an ambiguous profile is
// an error (exit 1).
func runDaily(args []string, globalJSON bool, stdout, stderr io.Writer, env environment) int {
	flags := flag.NewFlagSet("daily", flag.ContinueOnError)
	dateFlag := flags.String("date", "", "day to show, YYYY-MM-DD")
	jsonFlag := flags.Bool("json", false, "print JSON instead of human-readable text")
	if help, code := parseCommandFlags(flags, args, printDailyUsage, stdout, stderr); help || code != 0 {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s daily: unexpected argument %q\n\n", programName, flags.Arg(0))
		printDailyUsage(stderr)
		return exitUsage
	}
	date := strings.TrimSpace(*dateFlag)
	if date == "" {
		fmt.Fprintf(stderr, "%s daily: --date is required.\n\n", programName)
		printDailyUsage(stderr)
		return exitUsage
	}
	if err := validateDateValue(date); err != nil {
		fmt.Fprintf(stderr, "%s daily: invalid --date (%v).\n\n", programName, err)
		printDailyUsage(stderr)
		return exitUsage
	}
	jsonOut := globalJSON || *jsonFlag

	report := dailyReport{Command: "daily", Date: date, Records: []dailyRecord{}}
	emit := func(code int) int {
		emitReport(stdout, jsonOut, report, func(w io.Writer) { renderDailyHuman(w, report) })
		return code
	}
	fail := func(detail string) int {
		report.Status = "error"
		report.Detail = detail
		return emit(exitError)
	}
	noData := func(detail string) int {
		report.Status = "no_data"
		report.Detail = detail
		return emit(exitOK)
	}

	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if state == configInvalid {
		return fail(err.Error())
	}
	// A missing database is not an error: nothing has been imported yet.
	if _, statErr := os.Stat(cfg.databasePath); errors.Is(statErr, fs.ErrNotExist) {
		return noData(fmt.Sprintf("No local database at %s yet; this is normal on a clean install.", cfg.databasePath))
	} else if statErr != nil {
		return fail(fmt.Sprintf("Cannot access the database: %v", statErr))
	}

	db, err := openReadOnly(cfg.databasePath)
	if err != nil {
		return fail(fmt.Sprintf("Cannot open the database read-only: %v", err))
	}
	defer db.Close()

	version, err := schemaVersion(db)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the schema version: %v", err))
	}
	if version != expectedSchemaVersion {
		return fail(fmt.Sprintf(
			"Schema version %d cannot be read by this build (it reads version %d). The database was not modified.",
			version, expectedSchemaVersion))
	}
	hasTable, err := tableExists(db, activityTable)
	if err != nil {
		return fail(fmt.Sprintf("Cannot inspect the database: %v", err))
	}
	if !hasTable {
		return fail(fmt.Sprintf("The %s table is missing from the database.", activityTable))
	}

	// Profile: in export_file mode the configured export folder always wins,
	// so the answer does not depend on what else the database holds. In any
	// other mode the export_path is ignored — it can be stale after a source
	// switch — and only a single stored profile may be used, never one of
	// several. In cloud_session mode only cloud-origin rows take part: a
	// historical export row must be neither an ambiguity nor a result.
	if cfg.mode == modeExportFile && cfg.exportPath != "" {
		report.UserID = exportUserID(cfg.exportPath)
	} else {
		ids, err := distinctActivityUserIDs(db, activitySourceFilter(cfg.mode), 2)
		if err != nil {
			return fail(fmt.Sprintf("Cannot read profiles from the database: %v", err))
		}
		switch len(ids) {
		case 0:
			return noData("No daily activity found in the database.")
		case 1:
			report.UserID = ids[0]
		default:
			if cfg.mode == modeExportFile {
				return fail("The database holds several user profiles and no export folder is configured; " +
					"run `zepp-life setup --source export-file --export-dir DIR` so the profile can be derived deterministically.")
			}
			return fail("The database holds several user profiles and the configuration does not identify one; " +
				"`zepp-life daily` cannot choose between them.")
		}
	}

	rows, err := activityForDate(db, report.UserID, date, activitySourceFilter(cfg.mode))
	if err != nil {
		return fail(fmt.Sprintf("Cannot read daily activity: %v", err))
	}
	if len(rows) == 0 {
		return noData(fmt.Sprintf("No activity recorded for %s.", date))
	}
	for _, row := range rows {
		record := dailyRecord{
			Steps:      row.steps,
			DistanceM:  row.distanceM,
			ActiveKcal: row.activeKcal,
			DeviceKey:  row.deviceKey,
		}
		if row.deviceID.Valid {
			id := row.deviceID.String
			record.DeviceID = &id
		}
		report.Records = append(report.Records, record)
	}
	report.Status = "ok"
	return emit(exitOK)
}

func renderDailyHuman(w io.Writer, r dailyReport) {
	if r.Status == "error" {
		fmt.Fprintln(w, "Daily activity could not be read.")
		fmt.Fprintf(w, "  %-15s%s\n", "detail:", r.Detail)
		return
	}
	if r.Status != "ok" || len(r.Records) == 0 {
		fmt.Fprintf(w, "No daily activity for %s.\n", r.Date)
		if r.Detail != "" {
			fmt.Fprintf(w, "  %s\n", r.Detail)
		}
		return
	}
	fmt.Fprintf(w, "Daily activity %s\n", r.Date)
	fmt.Fprintf(w, "  %-15s%s\n", "profile:", r.UserID)
	for _, record := range r.Records {
		fmt.Fprintf(w, "  %-15s%s\n", "device:", deviceLabel(record))
		fmt.Fprintf(w, "    %-13s%d\n", "steps:", record.Steps)
		fmt.Fprintf(w, "    %-13s%.1f m\n", "distance:", record.DistanceM)
		fmt.Fprintf(w, "    %-13s%.1f kcal\n", "active kcal:", record.ActiveKcal)
	}
}

// deviceLabel names one row's device without ever merging rows: an absent
// device is shown as "-", not folded into any other row.
func deviceLabel(r dailyRecord) string {
	if r.DeviceID != nil && *r.DeviceID != "" {
		return *r.DeviceID
	}
	if r.DeviceKey != "" {
		return r.DeviceKey
	}
	return "-"
}

func printDailyUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  zepp-life daily --date YYYY-MM-DD [--json]

Prints the daily activity stored for one date, one block per device (devices
are never summed): the rows `+"`zepp-life sync --type daily_activity`"+` cached
from the Zepp cloud or imported from the export folder. Read-only: nothing
is created, changed or sent anywhere, and neither the keyring nor the
network is used.

Flags:
  --date    Day to show, YYYY-MM-DD (required)
  --json    Print JSON instead of human-readable text
`)
}
