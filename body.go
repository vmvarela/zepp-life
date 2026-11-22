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

// bodyReport is the stable JSON shape of `zepp-life body --latest-only`.
type bodyReport struct {
	Command      string       `json:"command"`
	Status       string       `json:"status"` // ok | no_data | profile_required | error
	Config       string       `json:"config"` // present | missing | invalid
	DatabasePath string       `json:"database_path"`
	UserID       string       `json:"user_id"`
	Detail       string       `json:"detail"`
	Measurement  *measurement `json:"measurement"`
}

func runBody(args []string, globalJSON bool, stdout, stderr io.Writer, env environment) int {
	flags := flag.NewFlagSet("body", flag.ContinueOnError)
	latestOnly := flags.Bool("latest-only", false, "show only the most recent measurement")
	jsonFlag := flags.Bool("json", false, "print JSON instead of human-readable text")
	if help, code := parseCommandFlags(flags, args, printBodyUsage, stdout, stderr); help || code != 0 {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s body: unexpected argument %q\n\n", programName, flags.Arg(0))
		printBodyUsage(stderr)
		return exitUsage
	}
	if !*latestOnly {
		fmt.Fprintf(stderr, "%s body: --latest-only is required; this build only shows the most recent measurement.\n\n", programName)
		printBodyUsage(stderr)
		return exitUsage
	}
	jsonOut := globalJSON || *jsonFlag

	report := bodyReport{Command: "body"}
	emit := func(code int) int {
		emitReport(stdout, jsonOut, report, func(w io.Writer) { renderBodyHuman(w, report) })
		return code
	}

	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	report.Config = string(state)
	if state == configInvalid {
		report.Status = "error"
		report.Detail = err.Error()
		return emit(exitError)
	}
	if state == configMissing {
		report.Detail = "No configuration file yet; this is normal on a clean install."
	}
	report.DatabasePath = cfg.databasePath

	if _, statErr := os.Stat(cfg.databasePath); errors.Is(statErr, fs.ErrNotExist) {
		report.Status = "no_data"
		report.Detail = joinDetail(report.Detail,
			fmt.Sprintf("No local database at %s yet; this is normal on a clean install.", cfg.databasePath))
		return emit(exitOK)
	}

	db, err := openReadOnly(cfg.databasePath)
	if err != nil {
		report.Status = "error"
		report.Detail = joinDetail(report.Detail, fmt.Sprintf("Cannot open the database read-only: %v", err))
		return emit(exitError)
	}
	defer db.Close()

	version, err := schemaVersion(db)
	if err != nil {
		report.Status = "error"
		report.Detail = joinDetail(report.Detail, fmt.Sprintf("Cannot read the schema version: %v", err))
		return emit(exitError)
	}
	if version != expectedSchemaVersion {
		report.Status = "error"
		report.Detail = joinDetail(report.Detail, fmt.Sprintf(
			"Schema version %d cannot be read by this build (it reads version %d). The database was not modified.",
			version, expectedSchemaVersion))
		return emit(exitError)
	}
	hasTable, err := bodyTableExists(db)
	if err != nil {
		report.Status = "error"
		report.Detail = joinDetail(report.Detail, fmt.Sprintf("Cannot inspect the database: %v", err))
		return emit(exitError)
	}
	if !hasTable {
		report.Status = "error"
		report.Detail = joinDetail(report.Detail,
			fmt.Sprintf("The %s table is missing from the database.", bodyTable))
		return emit(exitError)
	}

	// Profile: the stored user id first, otherwise the only profile the
	// database holds. In export_file mode the keyring is not consulted at
	// all — there are no cloud credentials — so the profile comes only from
	// the database.
	var userID string
	var found bool
	var keyErr error
	keyNote := ""
	if cfg.mode == modeExportFile {
		found = false
	} else {
		userID, found, keyErr = readCredential(env, keyringUserIDAccount)
		if keyErr != nil {
			keyNote = fmt.Sprintf("The stored user id could not be read (%v).", keyErr)
		}
	}
	if !found {
		ids, err := distinctUserIDs(db, 2)
		if err != nil {
			report.Status = "error"
			report.Detail = joinDetail(report.Detail, fmt.Sprintf("Cannot read profiles from the database: %v", err))
			return emit(exitError)
		}
		switch {
		case len(ids) == 1:
			userID = ids[0]
		case keyErr != nil:
			report.Status = "error"
			report.Detail = joinDetail(report.Detail, keyNote, "The profile to read could not be determined.")
			return emit(exitError)
		case len(ids) == 0:
			report.Status = "no_data"
			report.Detail = joinDetail(report.Detail, "No body measurements found in the database.")
			return emit(exitOK)
		default:
			report.Status = "profile_required"
			report.Detail = joinDetail(report.Detail,
				"The database holds several user profiles; select one to see its measurements. "+
					"Profile selection is not available in this build.")
			return emit(exitOK)
		}
	}
	report.UserID = userID
	if keyNote != "" {
		report.Detail = joinDetail(report.Detail, keyNote,
			"The only profile stored in the database is used instead.")
	}

	row, err := latestMeasurement(db, userID)
	if err != nil {
		report.Status = "error"
		report.Detail = joinDetail(report.Detail, fmt.Sprintf("Cannot read body measurements: %v", err))
		return emit(exitError)
	}
	if row == nil {
		report.Status = "no_data"
		report.Detail = joinDetail(report.Detail,
			fmt.Sprintf("No body measurements recorded for profile %s.", userID))
		return emit(exitOK)
	}
	report.Status = "ok"
	report.Measurement = row
	return emit(exitOK)
}

func renderBodyHuman(w io.Writer, r bodyReport) {
	if r.Status != "ok" || r.Measurement == nil {
		fmt.Fprintln(w, "No body measurement to show.")
		if r.Detail != "" {
			fmt.Fprintf(w, "  %s\n", r.Detail)
		}
		return
	}

	m := r.Measurement
	fmt.Fprintln(w, "Latest body measurement")
	fmt.Fprintf(w, "  %-17s%s\n", "profile:", r.UserID)
	fmt.Fprintf(w, "  %-17s%s\n", "timestamp:", m.Timestamp)
	fmt.Fprintf(w, "  %-17s%.1f kg\n", "weight:", m.WeightKg)
	printOptionalFloat(w, "bmi:", m.BMI, "%.1f")
	printOptionalFloat(w, "body fat:", m.BodyFatPct, "%.1f %%")
	printOptionalFloat(w, "muscle mass:", m.MuscleMassKg, "%.1f kg")
	printOptionalFloat(w, "water:", m.WaterPct, "%.1f %%")
	printOptionalFloat(w, "bone mass:", m.BoneMassKg, "%.1f kg")
	printOptionalInt(w, "visceral fat:", m.VisceralFatScore, "%d")
	printOptionalInt(w, "basal metabolism:", m.BasalMetabolismKcal, "%d kcal")
	printOptionalInt(w, "metabolic age:", m.MetabolicAge, "%d")
	if r.Detail != "" {
		fmt.Fprintf(w, "  %-17s%s\n", "note:", r.Detail)
	}
}

func printOptionalFloat(w io.Writer, label string, value *float64, format string) {
	if value != nil {
		fmt.Fprintf(w, "  %-17s%s\n", label, fmt.Sprintf(format, *value))
	}
}

func printOptionalInt(w io.Writer, label string, value *int64, format string) {
	if value != nil {
		fmt.Fprintf(w, "  %-17s%s\n", label, fmt.Sprintf(format, *value))
	}
}

func joinDetail(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}
