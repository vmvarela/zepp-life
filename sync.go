package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// typeBodyMeasurements is the only data type this build syncs from the cloud;
// typeDailyActivity (activity.go) is the only data type it imports offline.
const typeBodyMeasurements = "body_measurements"

// syncReport is the stable JSON shape of `zepp-life sync`.
type syncReport struct {
	Command   string `json:"command"`
	Status    string `json:"status"` // ok | error
	Type      string `json:"type"`
	Source    string `json:"source"` // cloud_session | export_file | ""
	UserID    string `json:"user_id"`
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Skipped   int    `json:"skipped"`
	Detail    string `json:"detail"`
}

// runSync downloads one data type from the Zepp cloud, or imports one data
// type from the configured export folder.
//
// Preconditions, all checked before any network request: --type is one of
// body_measurements or daily_activity, explicit dates are valid YYYY-MM-DD
// and ordered, and the configuration is readable. The source of a
// daily_activity sync is the configured mode and nothing else — cloud_session
// fetches summaries from the Zepp cloud, export_file re-reads the export
// folder offline — with no fallback between the two; date flags apply only
// to the cloud paths and are refused for the export folder, which re-reads
// everything on every run. The command never prompts.
//
// Postconditions on success (cloud): every fetched record that passes the
// inclusive date filter is upserted and, in the same transaction,
// sync_state records cursor=end_date with last_success. On failure the
// transaction is rolled back: no row changes, cursor and last_success stay
// untouched, and last_error/last_attempt are updated in a separate write.
//
// Invariant (cloud): added+updated+skipped equals the number of processed
// records, and the cursor advances only when the whole type succeeded.
// Termination: the body API answers with one bounded list of at most
// weightLimit items and the activity response is capped at maxResponseBytes;
// neither is paginated.
func runSync(args []string, globalJSON bool, stdout, stderr io.Writer, env environment) int {
	flags := flag.NewFlagSet("sync", flag.ContinueOnError)
	typeFlag := flags.String("type", "", "data type to sync")
	startFlag := flags.String("start-date", "", "first local date to keep, YYYY-MM-DD")
	endFlag := flags.String("end-date", "", "last local date to keep, YYYY-MM-DD")
	jsonFlag := flags.Bool("json", false, "print JSON instead of human-readable text")
	if help, code := parseCommandFlags(flags, args, printSyncUsage, stdout, stderr); help || code != 0 {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s sync: unexpected argument %q\n\n", programName, flags.Arg(0))
		printSyncUsage(stderr)
		return exitUsage
	}
	jsonOut := globalJSON || *jsonFlag

	// --- Preconditions: flag validation, before anything is opened. ---
	switch {
	case *typeFlag == "":
		fmt.Fprintf(stderr, "%s sync: --type is required; this build syncs %s and %s.\n\n",
			programName, typeBodyMeasurements, typeDailyActivity)
		printSyncUsage(stderr)
		return exitUsage
	case *typeFlag != typeBodyMeasurements && *typeFlag != typeDailyActivity:
		fmt.Fprintf(stderr, "%s sync: unsupported --type %q; this build supports %s and %s.\n\n",
			programName, *typeFlag, typeBodyMeasurements, typeDailyActivity)
		printSyncUsage(stderr)
		return exitUsage
	}
	if err := validateDateValue(*startFlag); err != nil {
		fmt.Fprintf(stderr, "%s sync: invalid --start-date (%v).\n\n", programName, err)
		printSyncUsage(stderr)
		return exitUsage
	}
	if err := validateDateValue(*endFlag); err != nil {
		fmt.Fprintf(stderr, "%s sync: invalid --end-date (%v).\n\n", programName, err)
		printSyncUsage(stderr)
		return exitUsage
	}

	report := syncReport{Command: "sync", Type: *typeFlag}
	emit := func(code int) int {
		emitReport(stdout, jsonOut, report, func(w io.Writer) { renderSyncHuman(w, report) })
		return code
	}

	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if state == configInvalid {
		report.Status = "error"
		report.Detail = err.Error()
		return emit(exitError)
	}
	if cfg.mode == modeCloudSession || cfg.mode == modeExportFile {
		report.Source = cfg.mode
	}

	if *typeFlag == typeDailyActivity {
		// The source is the configured mode, chosen by setup alone: no
		// prompt, no fallback between the two branches.
		switch cfg.mode {
		case modeExportFile:
			// The export import re-reads the whole folder on every run: a
			// date window would silently filter nothing persistent, so it
			// is refused here, where the mode is finally known.
			if *startFlag != "" || *endFlag != "" {
				fmt.Fprintf(stderr, "%s sync: --start-date and --end-date do not apply to %s in export_file mode; it re-reads the whole export folder every run.\n\n",
					programName, typeDailyActivity)
				printSyncUsage(stderr)
				return exitUsage
			}
			return runSyncExport(cfg, state, &report, emit)
		case modeCloudSession:
			return runSyncCloudDaily(cfg, &report, emit, *startFlag, *endFlag, stderr, env)
		default:
			report.Status = "error"
			report.Detail = "No configuration yet; run `zepp-life setup` first (cloud session) or " +
				"`zepp-life setup --source export-file --export-dir DIR` (export folder)."
			return emit(exitError)
		}
	}

	if cfg.mode != modeCloudSession {
		report.Status = "error"
		switch cfg.mode {
		case modeExportFile:
			report.Detail = "body_measurements comes from the Zepp cloud; your configuration uses export_file mode and was not changed. Run `zepp-life sync --type daily_activity` to import the folder instead."
		default:
			report.Detail = "No configuration yet; run `zepp-life setup` first."
		}
		return emit(exitError)
	}

	token, found, err := readCredential(env, keyringTokenAccount)
	if err != nil {
		report.Status = "error"
		report.Detail = fmt.Sprintf("Cannot read the stored Zepp token: %v", err)
		return emit(exitError)
	}
	if !found {
		report.Status = "error"
		report.Detail = "No stored Zepp token; run `zepp-life setup` first."
		return emit(exitError)
	}
	userID, hasUserID, err := readCredential(env, keyringUserIDAccount)
	if err != nil {
		report.Status = "error"
		report.Detail = fmt.Sprintf("Cannot read the stored user id: %v", err)
		return emit(exitError)
	}
	if hasUserID {
		report.UserID = userID
	}

	loc, err := time.LoadLocation(cfg.timezone)
	if err != nil {
		report.Status = "error"
		report.Detail = fmt.Sprintf("The configured timezone %q cannot be used: %v", cfg.timezone, err)
		return emit(exitError)
	}
	now := clockNow(env)

	// --- Window resolution, still without any network request. ---
	start, end := strings.TrimSpace(*startFlag), strings.TrimSpace(*endFlag)
	if end == "" {
		// The Python tool ends the window at the local system date.
		end = now.Format(dateLayout)
	}
	if start == "" && hasUserID {
		// Best-effort: any problem reading the cursor is reported with a
		// precise message when the database is opened right below.
		start = peekSyncCursor(cfg.databasePath, userID, typeBodyMeasurements)
	}
	if start == "" {
		start = defaultStartDate
	}
	if start > end {
		fmt.Fprintf(stderr, "%s sync: start date %s is after end date %s.\n\n",
			programName, start, end)
		printSyncUsage(stderr)
		return exitUsage
	}
	report.StartDate, report.EndDate = start, end

	// The database is opened before the network: a broken database must not
	// cost a request, and a failure of the attempt below can be recorded.
	db, _, err := openOrCreateDatabase(cfg.databasePath)
	if err != nil {
		report.Status = "error"
		report.Detail = err.Error()
		return emit(exitError)
	}
	defer db.Close()

	api := newCloudAPI(env, token)

	// recordFailure keeps the sync state honest after the attempt started.
	// Without a user id there is no row to key on, so nothing is written.
	recordFailure := func(cause error) int {
		if hasUserID {
			if stateErr := writeSyncFailure(db, userID, typeBodyMeasurements, cause.Error()); stateErr != nil {
				cause = fmt.Errorf("%v (the sync state could not be updated: %v)", cause, stateErr)
			}
		}
		report.Status = "error"
		report.Detail = cause.Error()
		return emit(exitError)
	}

	if err := api.probe(); err != nil {
		return recordFailure(err)
	}
	if !hasUserID {
		userID, err = api.discoverUserID(now.In(loc))
		if err != nil {
			return recordFailure(fmt.Errorf("%w; run `zepp-life setup --user-id <id>` to store it", err))
		}
		if err := writeCredential(env, keyringUserIDAccount, userID); err != nil {
			return recordFailure(fmt.Errorf("the user id was found but could not be stored: %w", err))
		}
		hasUserID = true
		report.UserID = userID
	}

	items, err := api.fetchWeightItems(userID)
	if err != nil {
		return recordFailure(err)
	}
	records, err := parseWeightItems(items, userID, loc, now, start, end)
	if err != nil {
		return recordFailure(err)
	}

	// One bounded transaction: rows and the success state commit together, so
	// a failure can never leave half of a sync behind.
	ctx, cancel := syncContext()
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return recordFailure(fmt.Errorf("cannot start the database transaction: %w", err))
	}
	added, updated, skipped := 0, 0, 0
	for i := range records {
		outcome, err := upsertBodyMeasurement(ctx, tx, &records[i], cfg.timezone)
		if err != nil {
			tx.Rollback()
			return recordFailure(fmt.Errorf("record %d could not be stored: %w", i+1, err))
		}
		switch outcome {
		case outcomeInserted:
			added++
		case outcomeUpdated:
			updated++
		default:
			skipped++
		}
	}
	// Invariant: the three counters partition exactly the processed records.
	if added+updated+skipped != len(records) {
		tx.Rollback()
		return recordFailure(fmt.Errorf("internal error: counted %d of %d records",
			added+updated+skipped, len(records)))
	}
	if err := writeSyncSuccess(ctx, tx, userID, typeBodyMeasurements, end, added+updated+skipped); err != nil {
		tx.Rollback()
		return recordFailure(fmt.Errorf("the sync state could not be updated: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return recordFailure(fmt.Errorf("the sync could not be committed: %w", err))
	}

	report.Status = "ok"
	report.Added, report.Updated, report.Skipped = added, updated, skipped
	return emit(exitOK)
}

// runSyncExport imports daily activity from the configured export folder.
//
// Preconditions (P): the configuration is a valid export_file with an
// absolute export_path that exists and holds recognizable activity CSVs
// within the import limits (256 candidate files, 64 MiB read, 100k rows),
// and the database holds (or is created with) the version 6 schema. Every
// row of every recognized CSV has a canonicalizable date and in-range
// metrics — a malformed row or an exceeded limit is reported before any
// write happens, because the whole tree is parsed first. This branch never
// reads the keyring, never prompts and never performs a request.
//
// Postconditions (Q): on success every parsed record is upserted exactly once
// on (user_id, date, device_key) inside a single transaction, the three
// counters partition the N parsed records (added+updated+skipped == N), and
// the commit makes all of them visible together. On any failure the
// transaction is rolled back: no row of this run survives, and the database
// keeps exactly the rows it had before.
//
// Invariant of the walk: after k of N records, the counters and the open
// transaction reflect exactly the prefix [0, k); the remaining bound N-k
// decreases on every iteration, so the walk terminates. Re-running the same
// folder yields skipped for unchanged rows: the import is idempotent.
func runSyncExport(cfg appConfig, state configState, report *syncReport, emit func(int) int) int {
	fail := func(detail string) int {
		report.Status = "error"
		report.Detail = detail
		return emit(exitError)
	}
	if state == configMissing || cfg.mode != modeExportFile {
		report.Status = "error"
		switch cfg.mode {
		case modeCloudSession:
			report.Detail = "daily_activity is imported from an export folder; your configuration uses cloud_session and was not changed. " +
				"Run `zepp-life setup --source export-file --export-dir DIR` to switch."
		default:
			report.Detail = "No configuration yet; run `zepp-life setup --source export-file --export-dir DIR` first."
		}
		return emit(exitError)
	}
	if cfg.exportPath == "" {
		return fail("No export folder is configured; run `zepp-life setup --source export-file --export-dir DIR` first.")
	}
	info, err := os.Stat(cfg.exportPath)
	if err != nil {
		return fail(fmt.Sprintf("The export folder %s cannot be read: %v; run `zepp-life setup --source export-file --export-dir DIR` to point at it again.", cfg.exportPath, err))
	}
	if !info.IsDir() {
		return fail(fmt.Sprintf("%s is not a folder; run `zepp-life setup --source export-file --export-dir DIR` to point at it again.", cfg.exportPath))
	}

	// --- Parse the whole compatible tree before opening the database. ---
	files, err := discoverActivityCSVs(cfg.exportPath)
	if err != nil {
		return fail(err.Error())
	}
	// One budget for the whole import: the file count is already bounded by
	// discovery, and every byte read and every row parsed is charged here
	// before it is retained, so the parse phase cannot grow without bound
	// and always finishes (or fails) before the database is opened.
	budget := &importBudget{}
	var records []activityRecord
	recognizedFiles := 0
	for _, path := range files {
		parsed, recognized, err := parseActivityFile(path, budget)
		if err != nil {
			// A malformed row or an exceeded limit fails the whole import:
			// no database file has been touched yet.
			return fail(err.Error())
		}
		if !recognized {
			continue
		}
		recognizedFiles++
		records = append(records, parsed...)
	}
	if recognizedFiles == 0 {
		return fail(fmt.Sprintf(
			"%s does not look like an activity export: no CSV there has a date column and a steps, distance or calories column.",
			cfg.exportPath))
	}

	userID := exportUserID(cfg.exportPath)
	report.UserID = userID
	n := len(records)

	// The database is opened after parsing: an invalid CSV must not even
	// create it.
	db, _, err := openOrCreateDatabase(cfg.databasePath)
	if err != nil {
		return fail(err.Error())
	}
	defer db.Close()

	// One transaction for the entire import: rows commit together or not at
	// all, so a failure can never leave half an import behind.
	ctx, cancel := syncContext()
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fail(fmt.Sprintf("cannot start the database transaction: %v", err))
	}
	added, updated, skipped := 0, 0, 0
	for k := range records {
		outcome, err := upsertDailyActivity(ctx, tx, userID, &records[k], exportActivitySource(userID, records[k].date))
		if err != nil {
			tx.Rollback()
			return fail(fmt.Sprintf("record %d (%s) could not be stored: %v", k+1, records[k].date, err))
		}
		switch outcome {
		case outcomeInserted:
			added++
		case outcomeUpdated:
			updated++
		default:
			skipped++
		}
		// Invariant: after k+1 records the counters cover exactly [0, k+1)
		// and the bound is n-(k+1) < n-k.
		if added+updated+skipped != k+1 {
			tx.Rollback()
			return fail(fmt.Sprintf("internal error: counted %d of %d records", added+updated+skipped, k+1))
		}
	}
	if added+updated+skipped != n {
		tx.Rollback()
		return fail(fmt.Sprintf("internal error: counted %d of %d records", added+updated+skipped, n))
	}
	if err := tx.Commit(); err != nil {
		return fail(fmt.Sprintf("the import could not be committed: %v", err))
	}

	report.Status = "ok"
	report.Added, report.Updated, report.Skipped = added, updated, skipped
	report.StartDate, report.EndDate = "", "" // the export import has no window
	latestDate := ""
	for _, record := range records {
		if record.date > latestDate {
			latestDate = record.date
		}
	}
	if latestDate == "" {
		report.Detail = fmt.Sprintf("No activity rows found in the export folder (%d file(s) checked).", len(files))
	} else {
		report.Detail = fmt.Sprintf("Next: zepp-life daily --date %s", latestDate)
	}
	return emit(exitOK)
}

// runSyncCloudDaily fetches daily activity summaries from the Zepp cloud.
//
// Preconditions (P): the configuration is a valid cloud_session (the caller
// routed on it), the stored token is present, the user id is stored or
// discovered and then stored, start/end are canonical ordered dates with the
// body-sync defaults (stored cursor or defaultStartDate, and today), and the
// response is one request bounded by maxResponseBytes under the shared 30s
// timeout without redirects. The database holds (or is created with) the
// version 6 schema.
//
// Postconditions (Q): on success exactly the valid in-range daily rows are
// upserted once on (user_id, date, device_key) and cursor=end_date with
// last_success commits in the same transaction as those rows. On any failure
// the row transaction rolls back — no row survives and cursor/last_success
// do not advance — and only last_error/last_attempt are written, on their
// own connection.
//
// Invariant: added+updated+skipped equals the number of processed rows, and
// the cursor advances only with a committed success. Termination: the
// response body is capped at maxResponseBytes and parseActivityData visits
// every entry a bounded number of times.
func runSyncCloudDaily(cfg appConfig, report *syncReport, emit func(int) int, startFlag, endFlag string, stderr io.Writer, env environment) int {
	fail := func(detail string) int {
		report.Status = "error"
		report.Detail = detail
		return emit(exitError)
	}
	loc, err := time.LoadLocation(cfg.timezone) // loadConfig already validated it
	if err != nil {
		return fail(fmt.Sprintf("The configured timezone %q cannot be used: %v", cfg.timezone, err))
	}
	now := clockNow(env)

	token, found, err := readCredential(env, keyringTokenAccount)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the stored Zepp token: %v", err))
	}
	if !found {
		return fail("No stored Zepp token; run `zepp-life setup` first.")
	}
	userID, hasUserID, err := readCredential(env, keyringUserIDAccount)
	if err != nil {
		return fail(fmt.Sprintf("Cannot read the stored user id: %v", err))
	}
	if hasUserID {
		report.UserID = userID
	}

	// --- Window resolution, still without any network request. Same
	// defaults as the body sync: today's end, the stored cursor (or the
	// default start) as the beginning. ---
	start, end := strings.TrimSpace(startFlag), strings.TrimSpace(endFlag)
	if end == "" {
		end = now.Format(dateLayout)
	}
	if start == "" && hasUserID {
		start = peekSyncCursor(cfg.databasePath, userID, typeDailyActivity)
	}
	if start == "" {
		start = defaultStartDate
	}
	if start > end {
		fmt.Fprintf(stderr, "%s sync: start date %s is after end date %s.\n\n",
			programName, start, end)
		printSyncUsage(stderr)
		return exitUsage
	}
	report.StartDate, report.EndDate = start, end

	// The database opens before the request: a broken database must not
	// cost a request, and a failed attempt can be recorded.
	db, _, err := openOrCreateDatabase(cfg.databasePath)
	if err != nil {
		return fail(err.Error())
	}
	defer db.Close()

	api := newCloudAPI(env, token)

	// recordFailure keeps the sync state honest after the attempt started.
	// Without a user id there is no row to key on, so nothing is written.
	recordFailure := func(cause error) int {
		if hasUserID {
			if stateErr := writeSyncFailure(db, userID, typeDailyActivity, cause.Error()); stateErr != nil {
				cause = fmt.Errorf("%v (the sync state could not be updated: %v)", cause, stateErr)
			}
		}
		report.Status = "error"
		report.Detail = cause.Error()
		return emit(exitError)
	}

	if !hasUserID {
		userID, err = api.discoverUserID(now.In(loc))
		if err != nil {
			return recordFailure(fmt.Errorf("%w; run `zepp-life setup --user-id <id>` to store it", err))
		}
		if err := writeCredential(env, keyringUserIDAccount, userID); err != nil {
			return recordFailure(fmt.Errorf("the user id was found but could not be stored: %w", err))
		}
		hasUserID = true
		report.UserID = userID
	}

	data, err := api.fetchActivityData(userID, start, end)
	if err != nil {
		return recordFailure(err)
	}
	records, err := parseActivityData(data, start, end)
	if err != nil {
		return recordFailure(err)
	}

	// One bounded transaction: rows and the success state commit together,
	// so a failure can never leave half of a sync behind.
	ctx, cancel := syncContext()
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return recordFailure(fmt.Errorf("cannot start the database transaction: %w", err))
	}
	added, updated, skipped := 0, 0, 0
	for i := range records {
		outcome, err := upsertDailyActivity(ctx, tx, userID, &records[i], cloudActivitySource(userID, records[i].date))
		if err != nil {
			tx.Rollback()
			return recordFailure(fmt.Errorf("record %d (%s) could not be stored: %w", i+1, records[i].date, err))
		}
		switch outcome {
		case outcomeInserted:
			added++
		case outcomeUpdated:
			updated++
		default:
			skipped++
		}
		// Invariant: after i+1 records the counters cover exactly [0, i+1).
		if added+updated+skipped != i+1 {
			tx.Rollback()
			return recordFailure(fmt.Errorf("internal error: counted %d of %d records",
				added+updated+skipped, i+1))
		}
	}
	// Invariant: the three counters partition exactly the processed records.
	if added+updated+skipped != len(records) {
		tx.Rollback()
		return recordFailure(fmt.Errorf("internal error: counted %d of %d records",
			added+updated+skipped, len(records)))
	}
	if err := writeSyncSuccess(ctx, tx, userID, typeDailyActivity, end, added+updated+skipped); err != nil {
		tx.Rollback()
		return recordFailure(fmt.Errorf("the sync state could not be updated: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return recordFailure(fmt.Errorf("the sync could not be committed: %w", err))
	}

	report.Status = "ok"
	report.Added, report.Updated, report.Skipped = added, updated, skipped
	latestDate := ""
	for _, record := range records {
		if record.date > latestDate {
			latestDate = record.date
		}
	}
	if latestDate != "" {
		report.Detail = fmt.Sprintf("Next: zepp-life daily --date %s", latestDate)
	}
	return emit(exitOK)
}

// validateDateValue accepts only canonical YYYY-MM-DD dates.
func validateDateValue(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(dateLayout, value)
	if err != nil || parsed.Format(dateLayout) != value {
		return fmt.Errorf("%q is not a YYYY-MM-DD date", value)
	}
	return nil
}

// peekSyncCursor reads the stored cursor of one data type without creating
// anything. It is best-effort on purpose: when it cannot read, the database
// is opened right afterwards and reports the real problem with a precise
// message, and a missing cursor only widens the window to the default start
// date.
func peekSyncCursor(path, userID, dataType string) string {
	db, err := openReadOnly(path)
	if err != nil {
		return ""
	}
	defer db.Close()
	ctx, cancel := syncContext()
	defer cancel()
	cursor, err := readSyncCursor(ctx, db, userID, dataType)
	if err != nil {
		return ""
	}
	return cursor
}

// readSyncCursor returns the inclusive cursor stored for one cloud data type
// of one profile.
func readSyncCursor(ctx context.Context, db *sql.DB, userID, dataType string) (string, error) {
	var cursor sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT cursor_date FROM sync_state
		 WHERE source_type = ? AND user_id = ? AND data_type = ?`,
		sourceCloudSession, userID, dataType,
	).Scan(&cursor)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", err
	}
	return cursor.String, nil
}

// writeSyncSuccess records a completed run of one data type: cursor and
// last_success advance together with the rows written in the same
// transaction.
func writeSyncSuccess(ctx context.Context, tx *sql.Tx, userID, dataType, endDate string, records int) error {
	_, err := tx.ExecContext(ctx, `
INSERT INTO sync_state (
	source_type, user_id, data_type, last_attempt_at, last_success_at,
	cursor_date, records_count, last_error
) VALUES (?, ?, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, ?, ?, NULL)
ON CONFLICT(source_type, user_id, data_type) DO UPDATE SET
	last_attempt_at = CURRENT_TIMESTAMP,
	last_success_at = CURRENT_TIMESTAMP,
	cursor_date = excluded.cursor_date,
	records_count = excluded.records_count,
	last_error = NULL`,
		sourceCloudSession, userID, dataType, endDate, records,
	)
	return err
}

// writeSyncFailure records a failed run of one data type on its own
// connection, after any row transaction was rolled back: cursor and
// last_success keep their previous values, only last_error and last_attempt
// change.
func writeSyncFailure(db *sql.DB, userID, dataType, detail string) error {
	ctx, cancel := syncContext()
	defer cancel()
	_, err := db.ExecContext(ctx, `
INSERT INTO sync_state (
	source_type, user_id, data_type, last_attempt_at, last_success_at,
	cursor_date, records_count, last_error
) VALUES (?, ?, ?, CURRENT_TIMESTAMP, NULL, NULL, 0, ?)
ON CONFLICT(source_type, user_id, data_type) DO UPDATE SET
	last_attempt_at = CURRENT_TIMESTAMP,
	records_count = 0,
	last_error = excluded.last_error`,
		sourceCloudSession, userID, dataType, detail,
	)
	return err
}

func renderSyncHuman(w io.Writer, r syncReport) {
	if r.Status != "ok" {
		fmt.Fprintln(w, "Sync did not run to completion.")
		fmt.Fprintf(w, "  %-15s%s\n", "profile:", orDash(r.UserID))
		fmt.Fprintf(w, "  %-15s%s\n", "detail:", r.Detail)
		return
	}
	rows := r.Added + r.Updated + r.Skipped
	switch {
	case r.Type != typeDailyActivity:
		fmt.Fprintln(w, "Synced body measurements")
	case r.Source == sourceCloudSession:
		// The heading names the source and whether anything changed.
		switch {
		case rows == 0:
			fmt.Fprintln(w, "No daily activity rows found in the Zepp cloud")
		case r.Added+r.Updated == 0:
			fmt.Fprintf(w, "No changes: all %d daily activity rows from the Zepp cloud are already up to date\n", r.Skipped)
		default:
			fmt.Fprintln(w, "Synced daily activity from the Zepp cloud")
		}
	default: // export_file: the offline folder import
		if rows == 0 {
			fmt.Fprintln(w, "No daily activity rows found")
		} else {
			fmt.Fprintln(w, "Imported daily activity from the export folder")
		}
	}
	fmt.Fprintf(w, "  %-15s%s\n", "profile:", r.UserID)
	if r.StartDate != "" {
		fmt.Fprintf(w, "  %-15s%s to %s\n", "range:", r.StartDate, r.EndDate)
	}
	fmt.Fprintf(w, "  %-15s%d\n", "added:", r.Added)
	fmt.Fprintf(w, "  %-15s%d\n", "updated:", r.Updated)
	fmt.Fprintf(w, "  %-15s%d\n", "skipped:", r.Skipped)
	if r.Type == typeDailyActivity && r.Detail != "" {
		fmt.Fprintln(w, r.Detail)
	}
}

func orDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func printSyncUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  zepp-life sync --type body_measurements [--start-date YYYY-MM-DD] [--end-date YYYY-MM-DD] [--json]
  zepp-life sync --type daily_activity [--start-date YYYY-MM-DD] [--end-date YYYY-MM-DD] [--json]

--type daily_activity uses the source configured by `+"`zepp-life setup`"+`,
with no fallback between the two:
  cloud_session   fetches daily summaries from the Zepp cloud in one bounded
                  request (date flags apply; defaults: stored cursor or
                  2020-01-01, and today)
  export_file     re-reads the whole export folder offline (date flags are
                  rejected; no keyring, no prompt, no network)

--type body_measurements always downloads from the Zepp cloud; never
prompts, the stored token from `+"`zepp-life setup`"+` is used, at most 200
records per run.

Flags:
  --type         Data type to sync (body_measurements or daily_activity)
  --start-date   First date to keep, YYYY-MM-DD (cloud syncs only; default:
                 the stored sync cursor, or 2020-01-01)
  --end-date     Last date to keep, YYYY-MM-DD (cloud syncs only; default:
                 today)
  --json         Print JSON instead of human-readable text
`)
}
