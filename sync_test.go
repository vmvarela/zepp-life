package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runSyncFor runs the supported sync command with the given extra flags.
func runSyncFor(t *testing.T, env environment, extraArgs ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"sync", "--type", typeBodyMeasurements}, extraArgs...)
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

// newCloudSyncEnv builds a cloud configuration pointing at a fresh database
// path and installs the stub as both API bases.
func newCloudSyncEnv(t *testing.T, secrets map[string]string, stub *cloudStub) environment {
	t.Helper()
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, env.databasePath)
	server := stub.install(&env)
	t.Cleanup(server.Close)
	return env
}

// weightItemJSON builds one synthetic cloud item with an in-window timestamp.
func weightItemJSON(id string, generatedTime any, summary map[string]any) map[string]any {
	return map[string]any{"id": id, "generatedTime": generatedTime, "summary": summary}
}

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := openReadWrite(path)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func countBodyRows(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM body_measurements").Scan(&count); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return count
}

type syncStateRow struct {
	cursor      sql.NullString
	lastSuccess sql.NullString
	lastAttempt sql.NullString
	lastError   sql.NullString
	records     int
}

func readSyncState(t *testing.T, db *sql.DB, userID string) (syncStateRow, bool) {
	t.Helper()
	var row syncStateRow
	err := db.QueryRow(`
SELECT cursor_date, CAST(last_success_at AS TEXT), CAST(last_attempt_at AS TEXT),
       last_error, records_count
FROM sync_state
WHERE source_type = ? AND user_id = ? AND data_type = ?`,
		sourceCloudSession, userID, typeBodyMeasurements,
	).Scan(&row.cursor, &row.lastSuccess, &row.lastAttempt, &row.lastError, &row.records)
	if err == sql.ErrNoRows {
		return syncStateRow{}, false
	}
	if err != nil {
		t.Fatalf("read sync state: %v", err)
	}
	return row, true
}

// seedSyncState writes a previous successful run into a fresh database.
func seedSyncState(t *testing.T, path, userID, cursor string) {
	t.Helper()
	db := openTestDB(t, path)
	if _, err := db.Exec(`
INSERT INTO sync_state (
	source_type, user_id, data_type, last_attempt_at, last_success_at,
	cursor_date, records_count, last_error
) VALUES (?, ?, ?, ?, ?, ?, 7, NULL)`,
		sourceCloudSession, userID, typeBodyMeasurements,
		"2026-01-01 00:00:00", "2026-01-01 00:00:00", cursor,
	); err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
}

func TestSyncBodyMeasurementsHappyPath(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	stub := &cloudStub{
		bandData: []any{map[string]any{"uid": 123456}},
		items: []any{
			weightItemJSON("w1", 1762066800, map[string]any{
				"weight": 71.5, "bmi": 22.1, "fatRate": 18.5, "muscleRate": 30.2,
				"bodyWaterRate": 55.4, "boneMass": 3.1, "visceralFat": 8,
				"metabolism": 1650, "muscleAge": 33,
			}),
			weightItemJSON("w2", 1762153200, map[string]any{"weight": 71.8}),
		},
	}
	env := newCloudSyncEnv(t, secrets, stub)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Added != 2 || report.Updated != 0 || report.Skipped != 0 {
		t.Fatalf("report = %+v", report)
	}
	if report.UserID != "123456" || report.StartDate != "2020-01-01" || report.EndDate != "2026-11-20" {
		t.Fatalf("report = %+v", report)
	}
	if secrets[keyringUserIDAccount] != "123456" {
		t.Errorf("discovered user id was not stored: %v", secrets)
	}

	// The exact endpoints: probe, discovery, then one bounded weight request.
	hits := stub.hits()
	if len(hits) != 3 {
		t.Fatalf("hits = %v, want three requests", hits)
	}
	if !strings.Contains(hits[0], "/v1/sport/run/history.json") ||
		!strings.Contains(hits[1], "/v1/data/band_data.json") ||
		!strings.Contains(hits[2], "/users/123456/members/-1/weightRecords") ||
		!strings.Contains(hits[2], "limit=200") {
		t.Fatalf("hits = %v", hits)
	}

	// The stored bytes match what the Python tool writes.
	db := openTestDB(t, env.databasePath)
	if count := countBodyRows(t, db); count != 2 {
		t.Fatalf("rows = %d, want 2", count)
	}
	var id, timestamp, localDate, deviceKey, sourceType, provider string
	var weight float64
	// CAST keeps the driver from re-formatting the TIMESTAMP column: the raw
	// stored bytes must stay byte-identical to what Python writes.
	if err := db.QueryRow(`
SELECT id, CAST(timestamp AS TEXT), local_date, device_key, source_type, provider, weight_kg
FROM body_measurements WHERE id = ?`, "cloud_weight_123456_w1",
	).Scan(&id, &timestamp, &localDate, &deviceKey, &sourceType, &provider, &weight); err != nil {
		t.Fatalf("read stored row: %v", err)
	}
	if timestamp != "2025-11-02T07:00:00+00:00" || localDate != "2025-11-02" {
		t.Errorf("timestamp = %q, local_date = %q", timestamp, localDate)
	}
	if deviceKey != "" || sourceType != sourceCloudSession || provider != providerZeppLife {
		t.Errorf("device_key = %q, source_type = %q, provider = %q", deviceKey, sourceType, provider)
	}
	if weight != 71.5 {
		t.Errorf("weight = %v", weight)
	}

	state, found := readSyncState(t, db, "123456")
	if !found {
		t.Fatal("sync_state row is missing")
	}
	if state.cursor.String != "2026-11-20" || !state.lastSuccess.Valid || state.lastError.Valid {
		t.Fatalf("sync state = %+v", state)
	}
	if state.records != 2 {
		t.Errorf("records_count = %d, want 2", state.records)
	}
}

func TestSyncIsIdempotentAcrossRuns(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		weightItemJSON("w1", 1762066800, map[string]any{"weight": 70}),
		weightItemJSON("w2", 1762153200, map[string]any{"weight": 71}),
	}}
	env := newCloudSyncEnv(t, secrets, stub)

	if code, _, errOut := runSyncFor(t, env, "--json"); code != exitOK {
		t.Fatalf("first run exit = %d (stderr: %s)", code, errOut)
	}
	// The same window is synced again: the stored cursor alone would narrow
	// the window to today, exactly like the Python tool does.
	code, out, errOut := runSyncFor(t, env, "--json", "--start-date", "2020-01-01")
	if code != exitOK {
		t.Fatalf("second run exit = %d (stderr: %s)", code, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Added != 0 || report.Updated != 0 || report.Skipped != 2 {
		t.Fatalf("second run report = %+v, want everything skipped", report)
	}
	db := openTestDB(t, env.databasePath)
	if count := countBodyRows(t, db); count != 2 {
		t.Fatalf("rows after two runs = %d, want no duplicates", count)
	}
	// The user id was already stored, so discovery never runs at all.
	if got := stub.countPaths("/v1/data/band_data.json"); got != 0 {
		t.Errorf("band_data was requested %d times, want 0", got)
	}
}

func TestSyncUpdatesCorrectedRowsPreservingID(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		weightItemJSON("w1", 1762066800, map[string]any{"weight": 70}),
	}}
	env := newCloudSyncEnv(t, secrets, stub)
	if code, _, errOut := runSyncFor(t, env, "--json"); code != exitOK {
		t.Fatalf("first run exit = %d (stderr: %s)", code, errOut)
	}

	// The cloud corrects the same record; the same window is re-synced.
	stub.items = []any{weightItemJSON("w1", 1762066800, map[string]any{"weight": 71.5})}
	code, out, errOut := runSyncFor(t, env, "--json", "--start-date", "2020-01-01")
	if code != exitOK {
		t.Fatalf("second run exit = %d (stderr: %s)", code, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Added != 0 || report.Updated != 1 || report.Skipped != 0 {
		t.Fatalf("report = %+v, want one corrected row", report)
	}

	db := openTestDB(t, env.databasePath)
	if count := countBodyRows(t, db); count != 1 {
		t.Fatalf("rows = %d, want the same single row", count)
	}
	var id string
	var weight float64
	if err := db.QueryRow("SELECT id, weight_kg FROM body_measurements").Scan(&id, &weight); err != nil {
		t.Fatal(err)
	}
	if id != "cloud_weight_123456_w1" {
		t.Errorf("id = %q, want the original id preserved", id)
	}
	if weight != 71.5 {
		t.Errorf("weight = %v, want the corrected value", weight)
	}
}

func TestSyncCountsPartitionProcessedRecords(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		weightItemJSON("in1", 1762066800, map[string]any{"weight": 70}),
		weightItemJSON("out", 946684800, map[string]any{"weight": 60}), // 2000-01-01: outside
		weightItemJSON("in2", 1762153200, map[string]any{"weight": 71}),
	}}
	env := newCloudSyncEnv(t, secrets, stub)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	// The out-of-window record is filtered, not processed: only the two
	// in-range records are partitioned into the counters.
	if report.Added+report.Updated+report.Skipped != 2 || report.Added != 2 {
		t.Fatalf("report = %+v", report)
	}
	if count := countBodyRows(t, openTestDB(t, env.databasePath)); count != 2 {
		t.Fatalf("rows = %d, want 2", count)
	}
}

func TestSyncEmptyResponseStillAdvancesTheCursor(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{}}
	env := newCloudSyncEnv(t, secrets, stub)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "ok" || report.Added+report.Updated+report.Skipped != 0 {
		t.Fatalf("report = %+v", report)
	}
	state, found := readSyncState(t, openTestDB(t, env.databasePath), "123456")
	if !found || state.cursor.String != "2026-11-20" {
		t.Fatalf("sync state = %+v, found = %v", state, found)
	}
}

func TestSyncRejectsInvalidDatesBeforeAnyRequest(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	tests := [][]string{
		{"--start-date", "2026-13-01"},
		{"--start-date", "01-02-2026"},
		{"--end-date", "2026-02-31"},
		{"--start-date", "2026-01-02", "--end-date", "2026-01-01"},
	}
	for _, extra := range tests {
		stub := &cloudStub{items: []any{}}
		env := newCloudSyncEnv(t, secrets, stub)
		code, _, errOut := runSyncFor(t, env, extra...)
		if code != exitUsage {
			t.Errorf("%v exit = %d, want %d", extra, code, exitUsage)
		}
		if hits := stub.hits(); len(hits) != 0 {
			t.Errorf("%v sent requests before validation: %v", extra, hits)
		}
		if !strings.Contains(errOut, "--") {
			t.Errorf("%v stderr does not explain the problem: %q", extra, errOut)
		}
	}
}

func TestSyncRejectsUnsupportedOrMissingType(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	stub := &cloudStub{}
	env := newCloudSyncEnv(t, secrets, stub)

	if code := run([]string{"sync"}, &strings.Builder{}, &strings.Builder{}, env); code != exitUsage {
		t.Errorf("missing --type exit = %d, want %d", code, exitUsage)
	}
	var stdout, stderr strings.Builder
	code := run([]string{"sync", "--type", "sleep"}, &stdout, &stderr, env)
	if code != exitUsage {
		t.Errorf("--type sleep exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), typeBodyMeasurements) {
		t.Errorf("stderr = %q, want it to name the supported type", stderr.String())
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("unsupported types must not send requests: %v", hits)
	}
}

func TestSyncRefusesExportFileConfigurationWithoutTouchingIt(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	stub := &cloudStub{}
	env := newTestEnvWithSecrets(t, secrets)
	writeConfigFile(t, env.configPath,
		`{"mode": "export_file", "export_path": `+
			jsonString(t, filepath.Join(t.TempDir(), "export"))+`, "timezone": "UTC"}`)
	server := stub.install(&env)
	defer server.Close()
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, modeExportFile) {
		t.Fatalf("report = %+v", report)
	}
	if after := snapshotConfig(t, env.configPath); after != before {
		t.Fatalf("config changed:\nbefore %q\nafter  %q", before, after)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSyncWithoutSetupExitsOneAndNeverPrompts(t *testing.T) {
	env := newTestEnv(t)
	writeValidConfig(t, env.configPath, env.databasePath)
	env.promptSecret = func(string) (string, error) {
		t.Fatal("sync must never prompt")
		return "", nil
	}
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "zepp-life setup") {
		t.Fatalf("report = %+v", report)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSyncLeavesOtherSchemaVersionsUntouched(t *testing.T) {
	path := newFixture(t, 5, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, path)
	server := stub.install(&env)
	defer server.Close()
	before := snapshot(t, path)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "not modified") {
		t.Fatalf("report = %+v", report)
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("database changed:\nbefore %q\nafter  %q", before, after)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSyncLeavesAnIncompleteDatabaseUntouched(t *testing.T) {
	// Version 6 but without the complete schema: reported, never repaired.
	path := newFixture(t, 6, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, path)
	server := stub.install(&env)
	defer server.Close()
	before := snapshot(t, path)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !strings.Contains(report.Detail, "complete version 6 schema") {
		t.Fatalf("report = %+v", report)
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("database changed:\nbefore %q\nafter  %q", before, after)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSyncFailureKeepsCursorAndRecordsTheError(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	if _, _, err := openOrCreateDatabase(path); err != nil {
		t.Fatalf("create database: %v", err)
	}
	seedSyncState(t, path, "123456", "2026-01-01")

	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		// 2026-01-01T00:00:00Z is 01:00 in Madrid: inside the seeded window.
		map[string]any{"id": "broken", "generatedTime": 1767225600, "summary": map[string]any{}},
	}}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, path)
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "weight") {
		t.Fatalf("report = %+v", report)
	}
	if report.Added+report.Updated+report.Skipped != 0 {
		t.Errorf("failed sync must not report stored counters: %+v", report)
	}

	db := openTestDB(t, path)
	if count := countBodyRows(t, db); count != 0 {
		t.Fatalf("rows = %d, want none: the transaction must roll back", count)
	}
	state, found := readSyncState(t, db, "123456")
	if !found {
		t.Fatal("sync state row is missing")
	}
	if state.cursor.String != "2026-01-01" || state.lastSuccess.String != "2026-01-01 00:00:00" {
		t.Fatalf("cursor/last_success changed: %+v", state)
	}
	if !state.lastAttempt.Valid || state.lastAttempt.String == "2026-01-01 00:00:00" {
		t.Errorf("last_attempt was not updated: %+v", state)
	}
	if !state.lastError.Valid || !strings.Contains(state.lastError.String, "weight") {
		t.Errorf("last_error was not recorded: %+v", state)
	}
}

func TestSyncFetchFailureKeepsCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	if _, _, err := openOrCreateDatabase(path); err != nil {
		t.Fatalf("create database: %v", err)
	}
	seedSyncState(t, path, "123456", "2026-01-01")

	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{weightCode: http.StatusInternalServerError}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, path)
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if !strings.Contains(report.Detail, "HTTP 500") {
		t.Fatalf("report = %+v", report)
	}
	state, found := readSyncState(t, openTestDB(t, path), "123456")
	if !found {
		t.Fatal("sync state row is missing")
	}
	if state.cursor.String != "2026-01-01" || state.lastSuccess.String != "2026-01-01 00:00:00" {
		t.Fatalf("cursor/last_success changed: %+v", state)
	}
	if !state.lastError.Valid || !strings.Contains(state.lastError.String, "HTTP 500") {
		t.Errorf("last_error was not recorded: %+v", state)
	}
}

func TestSyncUsesTheStoredCursorAsDefaultStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	if _, _, err := openOrCreateDatabase(path); err != nil {
		t.Fatalf("create database: %v", err)
	}
	seedSyncState(t, path, "123456", "2025-11-02")

	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{}}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, path)
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.StartDate != "2025-11-02" || report.EndDate != "2026-11-20" {
		t.Fatalf("window = %s..%s, want the stored cursor to today", report.StartDate, report.EndDate)
	}
}

func TestSyncExplicitWindowFiltersTheLocalDates(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		weightItemJSON("kept", 1762068600, map[string]any{"weight": 70}),    // 2025-11-02 Madrid
		weightItemJSON("dropped", 1762254000, map[string]any{"weight": 71}), // 2025-11-04 Madrid
	}}
	env := newCloudSyncEnv(t, secrets, stub)

	code, out, errOut := runSyncFor(t, env, "--json",
		"--start-date", "2025-11-02", "--end-date", "2025-11-03")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.StartDate != "2025-11-02" || report.EndDate != "2025-11-03" || report.Added != 1 {
		t.Fatalf("report = %+v", report)
	}
	if count := countBodyRows(t, openTestDB(t, env.databasePath)); count != 1 {
		t.Fatalf("rows = %d, want only the in-window record", count)
	}
}

func TestSyncKeepsUnchangedNullRowsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	if _, _, err := openOrCreateDatabase(path); err != nil {
		t.Fatalf("create database: %v", err)
	}
	db := openTestDB(t, path)
	// A row with NULL optional columns, written exactly like this sync would.
	if _, err := db.Exec(`
INSERT INTO body_measurements (
	id, provider, source_type, user_id, device_key, timezone, timestamp,
	local_date, weight_kg
) VALUES (?, ?, ?, ?, '', 'Europe/Madrid', ?, ?, 70)`,
		"cloud_weight_123456_w1", providerZeppLife, sourceCloudSession,
		"123456", "2025-11-02T07:00:00+00:00", "2025-11-02",
	); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	var seededUpdatedAt string
	if err := db.QueryRow("SELECT CAST(updated_at AS TEXT) FROM body_measurements").Scan(&seededUpdatedAt); err != nil {
		t.Fatal(err)
	}

	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		weightItemJSON("w1", 1762066800, map[string]any{"weight": 70}),
	}}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, path)
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	// NULL compares equal to NULL: the identical row is skipped, not
	// rewritten, so updated_at does not move on every run.
	if report.Skipped != 1 || report.Updated != 0 || report.Added != 0 {
		t.Fatalf("report = %+v", report)
	}
	var updatedAt string
	if err := openTestDB(t, path).QueryRow("SELECT CAST(updated_at AS TEXT) FROM body_measurements").Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt != seededUpdatedAt {
		t.Errorf("updated_at changed from %q to %q on an unchanged row", seededUpdatedAt, updatedAt)
	}
}

func TestSyncHumanOutputShowsTheCounters(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{
		weightItemJSON("w1", 1762066800, map[string]any{"weight": 70}),
	}}
	env := newCloudSyncEnv(t, secrets, stub)

	code, out, errOut := runSyncFor(t, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	for _, want := range []string{
		"Synced body measurements", "123456", "2020-01-01 to 2026-11-20",
		"added:", "updated:", "skipped:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestSyncJSONShapeIsStable(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	stub := &cloudStub{items: []any{}}
	env := newCloudSyncEnv(t, secrets, stub)

	code, out, errOut := runSyncFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	for _, field := range []string{
		"command", "status", "type", "user_id", "start_date", "end_date",
		"added", "updated", "skipped", "detail",
	} {
		if _, ok := raw[field]; !ok {
			t.Errorf("report is missing %q: %v", field, raw)
		}
	}
	if raw["command"] != "sync" || raw["type"] != typeBodyMeasurements || raw["status"] != "ok" {
		t.Fatalf("report = %v", raw)
	}
}

// snapshotConfig fingerprints a configuration file.
func snapshotConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	return string(data)
}
