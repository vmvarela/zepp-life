package main

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// This file covers `sync --type daily_activity` in cloud_session mode: the
// single bounded band_data request, every accepted payload shape, the
// database mapping, the cursor lifecycle and the `daily` readback. All
// servers are httptest and all databases live in t.TempDir; no test reaches
// the real service.

// activityRequest is one captured request to the synthetic cloud.
type activityRequest struct {
	method string
	path   string
	query  url.Values
	header http.Header
}

// activityStub serves /v1/data/band_data.json with a settable payload or
// status code and records every request. A request without a userid query
// parameter is the user-id lookup and is served discoveryPayload when set.
type activityStub struct {
	mu               sync.Mutex
	status           int // 0 means 200
	payload          any // value of {"data": ...} for summary requests
	discoveryPayload any // served when the request has no userid
	requests         []activityRequest
}

func (s *activityStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, activityRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.Query(),
		header: r.Header.Clone(),
	})
	status, payload := s.status, s.payload
	if r.URL.Query().Get("userid") == "" && s.discoveryPayload != nil {
		payload = s.discoveryPayload
	}
	s.mu.Unlock()

	if status != 0 && status != http.StatusOK {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, map[string]any{"data": payload})
}

func (s *activityStub) install(env *environment) *httptest.Server {
	server := httptest.NewServer(s)
	env.huamiBase = server.URL
	env.zeppBase = server.URL
	return server
}

func (s *activityStub) hits() []activityRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]activityRequest(nil), s.requests...)
}

func (s *activityStub) setPayload(payload any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.payload = payload
}

func (s *activityStub) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

// newCloudDailyEnv builds a cloud configuration with the token and the user
// id seeded in the fake keyring — so no discovery request happens — and the
// activity stub installed as both API bases.
func newCloudDailyEnv(t *testing.T, stub *activityStub) environment {
	t.Helper()
	env := newTestEnvWithSecrets(t, map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	})
	writeValidConfig(t, env.configPath, env.databasePath)
	server := stub.install(&env)
	t.Cleanup(server.Close)
	return env
}

// daySummary builds a stp day summary with the three mapped metrics.
func daySummary(steps int64, distanceM, kcal float64) map[string]any {
	return map[string]any{"stp": map[string]any{"ttl": steps, "dis": distanceM, "cal": kcal}}
}

// dayEntry builds one array entry with its date and summary.
func dayEntry(date string, summary any) map[string]any {
	return map[string]any{"date_time": date, "summary": summary}
}

// base64JSON encodes a value the way the API wraps nested payloads.
func base64JSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

// dailySyncState is the sync_state row keyed on the daily activity type.
type dailySyncState struct {
	cursor      sql.NullString
	lastSuccess sql.NullString
	lastError   sql.NullString
	records     int
}

func readDailySyncState(t *testing.T, dbPath, userID string) (dailySyncState, bool) {
	t.Helper()
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()
	var state dailySyncState
	err = db.QueryRow(`
SELECT cursor_date, CAST(last_success_at AS TEXT), last_error, records_count
FROM sync_state
WHERE source_type = ? AND user_id = ? AND data_type = ?`,
		sourceCloudSession, userID, typeDailyActivity,
	).Scan(&state.cursor, &state.lastSuccess, &state.lastError, &state.records)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return dailySyncState{}, false
	case err != nil:
		t.Fatalf("read sync_state: %v", err)
	}
	return state, true
}

// runCloudDailySync runs the daily sync against the stub's current state.
func runCloudDailySync(t *testing.T, env environment, extra ...string) (int, syncReport, string) {
	t.Helper()
	args := append([]string{"--type", typeDailyActivity, "--json"}, extra...)
	code, out, errOut := runSyncForType(t, env, args...)
	var report syncReport
	if out != "" {
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("output is not JSON: %v\n%s\nstderr: %s", err, out, errOut)
		}
	}
	return code, report, out + errOut
}

// --- one row, request contract, mapping, cursor success --------------------

func TestSyncDailyActivityCloudImportsOneRow(t *testing.T) {
	stub := &activityStub{payload: []any{dayEntry("2026-11-19", daySummary(8000, 6100.5, 310.2))}}
	env := newCloudDailyEnv(t, stub)

	code, report, out := runCloudDailySync(t, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
	}
	if report.Status != "ok" || report.Type != typeDailyActivity ||
		report.Source != sourceCloudSession || report.UserID != "123456" {
		t.Fatalf("report = %+v", report)
	}
	if report.Added != 1 || report.Updated != 0 || report.Skipped != 0 {
		t.Errorf("counters = %d/%d/%d, want 1/0/0", report.Added, report.Updated, report.Skipped)
	}
	// Body-sync window defaults: cursor-less start, today's end (fixed clock).
	if report.StartDate != "2020-01-01" || report.EndDate != "2026-11-20" {
		t.Errorf("window = %s..%s, want 2020-01-01..2026-11-20", report.StartDate, report.EndDate)
	}
	if strings.Contains(out, "synthetic-token") {
		t.Errorf("the JSON leaks the stored token:\n%s", out)
	}

	// Exactly one request: no probe, no discovery (the uid was seeded).
	hits := stub.hits()
	if len(hits) != 1 {
		t.Fatalf("requests = %d, want exactly 1: %v", len(hits), hits)
	}
	hit := hits[0]
	if hit.method != http.MethodGet || hit.path != "/v1/data/band_data.json" {
		t.Errorf("request = %s %s, want GET /v1/data/band_data.json", hit.method, hit.path)
	}
	for param, want := range map[string]string{
		"query_type":  "summary",
		"device_type": "android_phone",
		"userid":      "123456",
		"from_date":   "2020-01-01",
		"to_date":     "2026-11-20",
	} {
		if got := hit.query.Get(param); got != want {
			t.Errorf("query %s = %q, want %q (all: %v)", param, got, want, hit.query)
		}
	}
	for header, want := range map[string]string{
		"apptoken":     "synthetic-token",
		"appPlatform":  "web",
		"appname":      "com.xiaomi.hm.health",
		"Content-Type": "application/json",
	} {
		if got := hit.header.Get(header); got != want {
			t.Errorf("header %s = %q, want %q", header, got, want)
		}
	}

	// Row mapping: cloud identity, UTC, and every column the cloud does not
	// carry stored as NULL.
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want exactly one", rows)
	}
	row := rows[0]
	if row.id != "cloud_123456_2026-11-19" {
		t.Errorf("id = %q, want cloud_<uid>_<date>", row.id)
	}
	if row.provider != providerZeppLife || row.sourceType != sourceCloudSession || row.userID != "123456" {
		t.Errorf("provider/source/user = %q/%q/%q", row.provider, row.sourceType, row.userID)
	}
	if row.timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC", row.timezone)
	}
	if row.deviceID.Valid || row.deviceKey != "" {
		t.Errorf("device = %v/%q, want NULL and empty key", row.deviceID, row.deviceKey)
	}
	if row.sourceRecord.Valid || row.collectedAt.Valid ||
		row.totalKcal.Valid || row.floors.Valid || row.activeMin.Valid {
		t.Errorf("source_record_id/collected_at/total_kcal/floors/active_minutes must stay NULL: %+v", row)
	}
	if row.date != "2026-11-19" || row.steps != 8000 ||
		row.distanceM != 6100.5 || row.activeKcal != 310.2 {
		t.Errorf("mapped values = %+v", row)
	}

	// Cursor advanced with the rows, in the same transaction.
	state, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok {
		t.Fatal("sync_state row missing after a successful sync")
	}
	if !state.cursor.Valid || state.cursor.String != "2026-11-20" {
		t.Errorf("cursor = %v, want the end date", state.cursor)
	}
	if !state.lastSuccess.Valid {
		t.Error("last_success was not recorded")
	}
	if state.lastError.Valid {
		t.Errorf("last_error = %v, want NULL", state.lastError)
	}
	if state.records != 1 {
		t.Errorf("records_count = %d, want 1", state.records)
	}
}

// --- payload shapes ---------------------------------------------------------

func TestSyncDailyActivityCloudPayloadShapes(t *testing.T) {
	summary := daySummary(5000, 4000, 210)
	tests := []struct {
		name    string
		payload any
		date    string
	}{
		{
			name:    "base64 json array",
			payload: base64JSON(t, []any{dayEntry("2026-11-10", summary)}),
			date:    "2026-11-10",
		},
		{
			name:    "direct json array",
			payload: []any{dayEntry("2026-11-10", summary)},
			date:    "2026-11-10",
		},
		{
			name:    "base64 summary inside the entry",
			payload: []any{dayEntry("2026-11-10", base64JSON(t, summary))},
			date:    "2026-11-10",
		},
		{
			name:    "date field instead of date_time",
			payload: []any{map[string]any{"date": "2026-11-10", "summary": summary}},
			date:    "2026-11-10",
		},
		{
			name:    "datetime date_time",
			payload: []any{map[string]any{"date_time": "2026-11-10 09:30:00", "summary": summary}},
			date:    "2026-11-10",
		},
		{
			name:    "date-keyed object with entry values",
			payload: map[string]any{"2026-11-10": map[string]any{"summary": summary}},
			date:    "2026-11-10",
		},
		{
			name:    "date-keyed object with inline summary",
			payload: map[string]any{"2026-11-10": summary},
			date:    "2026-11-10",
		},
		{
			name:    "date-keyed object with base64 summary",
			payload: map[string]any{"2026-11-10": map[string]any{"summary": base64JSON(t, summary)}},
			date:    "2026-11-10",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stub := &activityStub{payload: test.payload}
			env := newCloudDailyEnv(t, stub)

			code, report, out := runCloudDailySync(t, env)
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
			}
			if report.Added != 1 {
				t.Fatalf("added = %d, want 1 (report: %+v)", report.Added, report)
			}
			rows := readActivityRows(t, env.databasePath)
			if len(rows) != 1 {
				t.Fatalf("rows = %+v, want one", rows)
			}
			if rows[0].date != test.date || rows[0].steps != 5000 ||
				rows[0].distanceM != 4000 || rows[0].activeKcal != 210 {
				t.Errorf("row = %+v, want the mapped summary on %s", rows[0], test.date)
			}
			if rows[0].sourceType != sourceCloudSession || rows[0].id != "cloud_123456_"+test.date {
				t.Errorf("identity = %q/%q", rows[0].id, rows[0].sourceType)
			}
		})
	}
}

// --- range filter and date flags -------------------------------------------

func TestSyncDailyActivityCloudFiltersToTheRequestedRange(t *testing.T) {
	stub := &activityStub{payload: []any{
		dayEntry("2026-10-31", daySummary(1, 1, 1)), // before the window
		dayEntry("2026-11-01", daySummary(100, 10, 10)),
		dayEntry("2026-11-03", daySummary(300, 30, 30)),
		dayEntry("2026-11-04", daySummary(4, 4, 4)), // after the window
	}}
	env := newCloudDailyEnv(t, stub)

	// Date flags apply to the cloud source.
	code, report, out := runCloudDailySync(t, env, "--start-date", "2026-11-01", "--end-date", "2026-11-03")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
	}
	if report.Added != 2 {
		t.Fatalf("added = %d, want only the two in-range days (report: %+v)", report.Added, report)
	}
	hits := stub.hits()
	if len(hits) != 1 {
		t.Fatalf("requests = %d, want 1", len(hits))
	}
	if got := hits[0].query.Get("from_date"); got != "2026-11-01" {
		t.Errorf("from_date = %q, want the flag value", got)
	}
	if got := hits[0].query.Get("to_date"); got != "2026-11-03" {
		t.Errorf("to_date = %q, want the flag value", got)
	}
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want the two in-range days", rows)
	}
	for _, row := range rows {
		if row.date < "2026-11-01" || row.date > "2026-11-03" {
			t.Errorf("stored out-of-range date %q", row.date)
		}
	}
}

func TestSyncDailyActivityCloudRejectsInvertedDateWindow(t *testing.T) {
	stub := &activityStub{payload: []any{}}
	env := newCloudDailyEnv(t, stub)

	code, _, out := runCloudDailySync(t, env, "--start-date", "2026-11-05", "--end-date", "2026-11-01")
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitUsage, out)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent before the window check: %v", hits)
	}
}

// --- empty result, skips, malformed payloads -------------------------------

func TestSyncDailyActivityCloudEmptyResultAdvancesCursor(t *testing.T) {
	for name, payload := range map[string]any{
		"empty array": []any{},
		"null data":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &activityStub{payload: payload}
			env := newCloudDailyEnv(t, stub)

			code, report, out := runCloudDailySync(t, env)
			if code != exitOK {
				t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
			}
			if report.Added+report.Updated+report.Skipped != 0 {
				t.Fatalf("counters = %+v, want an empty result", report)
			}
			if rows := readActivityRows(t, env.databasePath); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none", rows)
			}
			state, ok := readDailySyncState(t, env.databasePath, "123456")
			if !ok {
				t.Fatal("an empty success must still advance the cursor")
			}
			if !state.cursor.Valid || state.cursor.String != "2026-11-20" || !state.lastSuccess.Valid {
				t.Errorf("state = %+v, want cursor=end and last_success set", state)
			}
		})
	}
}

func TestSyncDailyActivityCloudSkipsEntriesWithoutUsableActivity(t *testing.T) {
	stub := &activityStub{payload: []any{
		dayEntry("2026-11-10", map[string]any{"other": 1}),                      // valid summary without stp
		dayEntry("2026-11-11", map[string]any{"stp": map[string]any{}}),         // empty stp object
		dayEntry("2026-11-12", map[string]any{"stp": nil}),                      // falsy stp
		dayEntry("2026-11-13", map[string]any{"stp": false}),                    // falsy stp
		dayEntry("2026-11-14", map[string]any{"stp": 0}),                        // falsy stp
		dayEntry("2026-11-15", map[string]any{"stp": []any{}}),                  // falsy stp
		dayEntry("2026-11-16", daySummary(100, 50, 10)),                         // usable
		dayEntry("2026-11-17", map[string]any{"stp": map[string]any{"ttl": 7}}), // missing metrics default to 0
		dayEntry("2026-11-18", nil),                                             // summary is null
		dayEntry("2026-11-18", ""),                                              // summary is empty
		map[string]any{"date_time": "2026-11-19"},                               // summary field absent
	}}
	env := newCloudDailyEnv(t, stub)

	code, report, out := runCloudDailySync(t, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
	}
	if report.Added != 2 {
		t.Fatalf("added = %d, want 2 usable days (report: %+v)", report.Added, report)
	}
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 2 {
		t.Fatalf("rows = %+v, want two", rows)
	}
	byDate := map[string]storedActivityRow{}
	for _, row := range rows {
		byDate[row.date] = row
	}
	if row, ok := byDate["2026-11-17"]; !ok || row.steps != 7 ||
		row.distanceM != 0 || row.activeKcal != 0 {
		t.Errorf("missing metrics row = %+v, want steps 7 and zeros", row)
	}
	if _, ok := byDate["2026-11-10"]; ok {
		t.Error("an entry without stp was stored")
	}
	if _, ok := byDate["2026-11-19"]; ok {
		t.Error("an entry without a summary was stored")
	}

	// Days with no usable activity are not failures: the sync succeeds and
	// the cursor advances to the end of the window.
	state, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok {
		t.Fatal("sync_state missing after a successful sync")
	}
	if !state.cursor.Valid || state.cursor.String != "2026-11-20" || !state.lastSuccess.Valid {
		t.Errorf("state = %+v, want cursor=end and last_success", state)
	}
	if state.lastError.Valid {
		t.Errorf("last_error = %v, want NULL after a successful sync", state.lastError)
	}
}

func TestSyncDailyActivityCloudMalformedFailsAndRecordsTheError(t *testing.T) {
	summary := daySummary(10, 10, 10)
	tests := map[string]any{
		"data is a number":         123,
		"data is not base64":       "%%%",
		"data is base64 garbage":   base64.StdEncoding.EncodeToString([]byte("hello")),
		"entry is not an object":   []any{"just-a-string"},
		"entry without a date":     []any{map[string]any{"summary": summary}},
		"entry date is not a date": []any{map[string]any{"date_time": "banana", "summary": summary}},
		"entry date is a number":   []any{map[string]any{"date_time": 1763000000, "summary": summary}},
		"null metric":              []any{dayEntry("2026-11-10", map[string]any{"stp": map[string]any{"ttl": nil}})},
		"malformed metric":         []any{dayEntry("2026-11-10", map[string]any{"stp": map[string]any{"ttl": "abc"}})},
		"boolean metric":           []any{dayEntry("2026-11-10", map[string]any{"stp": map[string]any{"ttl": true}})},
		"negative metric":          []any{dayEntry("2026-11-10", map[string]any{"stp": map[string]any{"ttl": -5}})},
		"out of range metric":      []any{dayEntry("2026-11-10", map[string]any{"stp": map[string]any{"dis": 1e12}})},
		"nan metric":               []any{dayEntry("2026-11-10", map[string]any{"stp": map[string]any{"ttl": "NaN"}})},
		"date key is not a date":   map[string]any{"banana": map[string]any{"summary": summary}},
		// Present-but-malformed summaries abort the run: they must never be
		// skipped while the cursor could advance past their date.
		"summary is not base64":       []any{dayEntry("2026-11-10", "!!!not-base64")},
		"summary decodes to a string": []any{dayEntry("2026-11-10", base64JSON(t, "not an object"))},
		"summary decodes to an array": []any{dayEntry("2026-11-10", base64JSON(t, []any{1, 2}))},
		"summary decodes to bad JSON": []any{dayEntry("2026-11-10", base64.StdEncoding.EncodeToString([]byte("{oops")))},
		"summary decodes to null":     []any{dayEntry("2026-11-10", base64.StdEncoding.EncodeToString([]byte("null")))},
		"summary is a number":         []any{dayEntry("2026-11-10", 5)},
		"summary is a boolean":        []any{dayEntry("2026-11-10", true)},
		"summary is an array":         []any{dayEntry("2026-11-10", []any{1, 2})},
		"stp is a truthy scalar":      []any{dayEntry("2026-11-10", map[string]any{"stp": 5})},
		"stp is a string":             []any{dayEntry("2026-11-10", map[string]any{"stp": "abc"})},
		"stp is a non-empty array":    []any{dayEntry("2026-11-10", map[string]any{"stp": []any{1}})},
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			stub := &activityStub{payload: payload}
			env := newCloudDailyEnv(t, stub)

			code, report, out := runCloudDailySync(t, env)
			if code != exitError {
				t.Fatalf("exit = %d, want %d (output: %s)", code, exitError, out)
			}
			if report.Status != "error" || report.Detail == "" {
				t.Fatalf("report = %+v, want an actionable error", report)
			}
			// Errors name the date, the field and the limit — never a
			// metric value or any payload content.
			if strings.Contains(report.Detail, "!!!") || strings.Contains(report.Detail, "eyJ") {
				t.Errorf("detail leaks payload content: %q", report.Detail)
			}
			// Nothing partial is stored, and the failure is recorded with
			// the cursor left untouched.
			if rows := readActivityRows(t, env.databasePath); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none after a failed sync", rows)
			}
			state, ok := readDailySyncState(t, env.databasePath, "123456")
			if !ok {
				t.Fatal("the failure was not recorded in sync_state")
			}
			if state.cursor.Valid || state.lastSuccess.Valid {
				t.Errorf("cursor/last_success = %v/%v, want both untouched", state.cursor, state.lastSuccess)
			}
			if !state.lastError.Valid || state.lastError.String == "" {
				t.Error("last_error was not recorded")
			}
		})
	}
}

// An in-range day whose summary is malformed must fail the run before any
// write: the rows and the cursor of the previous success stay exactly as
// they were, and the failure is recorded instead of skipping past the date.
func TestSyncDailyActivityCloudMalformedSummaryKeepsPriorState(t *testing.T) {
	stub := &activityStub{payload: []any{dayEntry("2026-11-10", daySummary(100, 50, 10))}}
	env := newCloudDailyEnv(t, stub)
	window := []string{"--start-date", "2026-11-01", "--end-date", "2026-11-30"}

	if code, report, out := runCloudDailySync(t, env, window...); code != exitOK {
		t.Fatalf("seed run: exit = %d, report = %+v (output: %s)", code, report, out)
	}
	beforeState, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok || !beforeState.cursor.Valid || !beforeState.lastSuccess.Valid {
		t.Fatalf("seed state = %+v, want cursor and last_success", beforeState)
	}
	beforeRows := readActivityRows(t, env.databasePath)
	if len(beforeRows) != 1 || beforeRows[0].date != "2026-11-10" {
		t.Fatalf("seed rows = %+v, want the one imported day", beforeRows)
	}

	stub.setPayload([]any{
		dayEntry("2026-11-10", daySummary(100, 50, 10)),
		dayEntry("2026-11-11", "!!!not-base64"), // in range, malformed
	})
	code, report, out := runCloudDailySync(t, env, window...)
	if code != exitError {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitError, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "base64") {
		t.Errorf("detail = %q, want the malformed-summary report", report.Detail)
	}
	if strings.Contains(report.Detail, "!!!") {
		t.Errorf("detail leaks payload content: %q", report.Detail)
	}

	afterState, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok {
		t.Fatal("sync_state disappeared")
	}
	if afterState.cursor.String != beforeState.cursor.String ||
		afterState.lastSuccess.String != beforeState.lastSuccess.String {
		t.Errorf("cursor/last_success moved from %q/%q to %q/%q",
			beforeState.cursor.String, beforeState.lastSuccess.String,
			afterState.cursor.String, afterState.lastSuccess.String)
	}
	if !afterState.lastError.Valid || !strings.Contains(afterState.lastError.String, "base64") {
		t.Errorf("last_error = %v, want the malformed-summary failure", afterState.lastError)
	}
	afterRows := readActivityRows(t, env.databasePath)
	if len(afterRows) != 1 || afterRows[0].date != "2026-11-10" {
		t.Fatalf("rows = %+v, want only the prior day and no partial write", afterRows)
	}
}

// A malformed summary outside the requested window is service overreturn: it
// is ignored, and the successful run still advances the cursor.
func TestSyncDailyActivityCloudOutOfRangeMalformedSummaryIsIgnored(t *testing.T) {
	stub := &activityStub{payload: []any{
		dayEntry("2026-10-01", "!!!not-base64"),                // before the window
		dayEntry("2026-11-10", daySummary(100, 50, 10)),        // in range
		dayEntry("2026-11-11", base64JSON(t, "not an object")), // after the window
	}}
	env := newCloudDailyEnv(t, stub)
	window := []string{"--start-date", "2026-11-01", "--end-date", "2026-11-10"}

	code, report, out := runCloudDailySync(t, env, window...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
	}
	if report.Added != 1 {
		t.Fatalf("added = %d, want the single in-range day (report: %+v)", report.Added, report)
	}
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 1 || rows[0].date != "2026-11-10" {
		t.Fatalf("rows = %+v, want only the in-range day", rows)
	}
	state, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok || !state.cursor.Valid || state.cursor.String != "2026-11-10" {
		t.Fatalf("state = %+v, want the cursor advanced to the end date", state)
	}
	if !state.lastSuccess.Valid {
		t.Error("last_success was not recorded")
	}
	if state.lastError.Valid {
		t.Errorf("last_error = %v, want NULL after a successful sync", state.lastError)
	}
}

func TestSyncDailyActivityCloudNon200Statuses(t *testing.T) {
	for status, wantDetail := range map[int]string{
		http.StatusUnauthorized:        "replace-token",
		http.StatusInternalServerError: "HTTP 500",
	} {
		t.Run(wantDetail, func(t *testing.T) {
			stub := &activityStub{payload: []any{}, status: status}
			env := newCloudDailyEnv(t, stub)

			code, report, out := runCloudDailySync(t, env)
			if code != exitError {
				t.Fatalf("exit = %d, want %d (output: %s)", code, exitError, out)
			}
			if !strings.Contains(report.Detail, wantDetail) {
				t.Errorf("detail = %q, want it to mention %q", report.Detail, wantDetail)
			}
			if rows := readActivityRows(t, env.databasePath); len(rows) != 0 {
				t.Fatalf("rows = %+v, want none", rows)
			}
		})
	}
}

func TestSyncDailyActivityCloudBoundedResponse(t *testing.T) {
	// A body past the 8 MiB cap is cut off mid-payload and reported, never
	// buffered in full.
	stub := &activityStub{payload: strings.Repeat("x", maxResponseBytes+4096)}
	env := newCloudDailyEnv(t, stub)

	code, report, out := runCloudDailySync(t, env)
	if code != exitError {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitError, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "not usable") {
		t.Errorf("detail = %q, want the bounded-response report", report.Detail)
	}
	if rows := readActivityRows(t, env.databasePath); len(rows) != 0 {
		t.Fatalf("rows = %+v, want none", rows)
	}
}

// --- retry, idempotence, failure -------------------------------------------

func TestSyncDailyActivityCloudRetryIsIdempotent(t *testing.T) {
	stub := &activityStub{payload: []any{
		dayEntry("2026-11-10", daySummary(100, 50, 10)),
		dayEntry("2026-11-11", daySummary(200, 60, 20)),
	}}
	env := newCloudDailyEnv(t, stub)
	// An explicit window keeps the cursor from sliding past the payload
	// days: after a success the cursor sits at the end date, so a repeat
	// with default dates would ask for [today, today] only.
	window := []string{"--start-date", "2026-11-01", "--end-date", "2026-11-30"}

	code, first, out := runCloudDailySync(t, env, window...)
	if code != exitOK || first.Added != 2 {
		t.Fatalf("first run: exit = %d, added = %d (output: %s)", code, first.Added, out)
	}
	ids := map[string]string{}
	for _, row := range readActivityRows(t, env.databasePath) {
		ids[row.date] = row.id
	}

	// Same payload again: nothing changes and no new ids appear.
	code, second, out := runCloudDailySync(t, env, window...)
	if code != exitOK {
		t.Fatalf("second run: exit = %d (output: %s)", code, out)
	}
	if second.Added != 0 || second.Updated != 0 || second.Skipped != 2 {
		t.Fatalf("second counters = %d/%d/%d, want 0/0/2", second.Added, second.Updated, second.Skipped)
	}
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if ids[row.date] != row.id {
			t.Errorf("id of %s changed: %q -> %q", row.date, ids[row.date], row.id)
		}
	}

	// A corrected day updates in place and keeps its primary key.
	stub.setPayload([]any{
		dayEntry("2026-11-10", daySummary(150, 50, 10)),
		dayEntry("2026-11-11", daySummary(200, 60, 20)),
	})
	code, third, out := runCloudDailySync(t, env, window...)
	if code != exitOK {
		t.Fatalf("third run: exit = %d (output: %s)", code, out)
	}
	if third.Added != 0 || third.Updated != 1 || third.Skipped != 1 {
		t.Fatalf("third counters = %d/%d/%d, want 0/1/1", third.Added, third.Updated, third.Skipped)
	}
	rows = readActivityRows(t, env.databasePath)
	if len(rows) != 2 {
		t.Fatalf("rows after update = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if ids[row.date] != row.id {
			t.Errorf("id of %s changed on update: %q -> %q", row.date, ids[row.date], row.id)
		}
		if row.date == "2026-11-10" && row.steps != 150 {
			t.Errorf("steps = %d, want the corrected 150", row.steps)
		}
	}
}

func TestSyncDailyActivityCloudFailurePreservesCursorAndRows(t *testing.T) {
	stub := &activityStub{payload: []any{dayEntry("2026-11-10", daySummary(100, 50, 10))}}
	env := newCloudDailyEnv(t, stub)

	if code, report, out := runCloudDailySync(t, env); code != exitOK {
		t.Fatalf("seed run: exit = %d, report = %+v (output: %s)", code, report, out)
	}
	beforeState, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok || !beforeState.cursor.Valid {
		t.Fatalf("seed state = %+v, want a cursor", beforeState)
	}
	beforeRows := readActivityRows(t, env.databasePath)

	stub.setStatus(http.StatusInternalServerError)
	code, report, out := runCloudDailySync(t, env)
	if code != exitError {
		t.Fatalf("failing run: exit = %d, want %d (output: %s)", code, exitError, out)
	}
	if !strings.Contains(report.Detail, "HTTP 500") {
		t.Errorf("detail = %q, want the status", report.Detail)
	}

	afterState, ok := readDailySyncState(t, env.databasePath, "123456")
	if !ok {
		t.Fatal("sync_state disappeared")
	}
	if afterState.cursor.String != beforeState.cursor.String ||
		afterState.lastSuccess.String != beforeState.lastSuccess.String {
		t.Errorf("cursor/last_success moved from %q/%q to %q/%q",
			beforeState.cursor.String, beforeState.lastSuccess.String,
			afterState.cursor.String, afterState.lastSuccess.String)
	}
	if !afterState.lastError.Valid || !strings.Contains(afterState.lastError.String, "HTTP 500") {
		t.Errorf("last_error = %v, want the HTTP 500 report", afterState.lastError)
	}
	afterRows := readActivityRows(t, env.databasePath)
	if len(afterRows) != len(beforeRows) {
		t.Fatalf("rows changed from %d to %d", len(beforeRows), len(afterRows))
	}
	for i := range afterRows {
		if afterRows[i] != beforeRows[i] {
			t.Errorf("row %d changed: %+v -> %+v", i, beforeRows[i], afterRows[i])
		}
	}
}

// --- user id discovery when it is not seeded --------------------------------

func TestSyncDailyActivityCloudDiscoversUserIDWhenMissing(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	stub := &activityStub{
		payload:          []any{dayEntry("2026-11-10", daySummary(42, 10, 5))},
		discoveryPayload: []any{map[string]any{"uid": "123456"}},
	}
	env := newTestEnvWithSecrets(t, secrets)
	writeValidConfig(t, env.configPath, env.databasePath)
	server := stub.install(&env)
	defer server.Close()

	code, report, out := runCloudDailySync(t, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (output: %s)", code, exitOK, out)
	}
	if report.UserID != "123456" {
		t.Errorf("user_id = %q, want the discovered uid", report.UserID)
	}
	if got := secrets[keyringUserIDAccount]; got != "123456" {
		t.Errorf("keyring user id = %q, want the discovered uid stored", got)
	}
	hits := stub.hits()
	if len(hits) != 2 {
		t.Fatalf("requests = %d, want lookup then summary: %v", len(hits), hits)
	}
	if hits[0].query.Get("userid") != "" {
		t.Errorf("the lookup already knew the uid: %v", hits[0].query)
	}
	if hits[1].query.Get("userid") != "123456" {
		t.Errorf("summary userid = %q, want the discovered uid", hits[1].query.Get("userid"))
	}
	if rows := readActivityRows(t, env.databasePath); len(rows) != 1 || rows[0].userID != "123456" {
		t.Fatalf("rows = %+v, want one row of the discovered profile", rows)
	}
}

// --- human output -----------------------------------------------------------

func TestSyncDailyActivityCloudHumanOutput(t *testing.T) {
	stub := &activityStub{payload: []any{dayEntry("2026-11-10", daySummary(100, 50, 10))}}
	env := newCloudDailyEnv(t, stub)

	window := []string{"--start-date", "2026-11-01", "--end-date", "2026-11-30"}
	code, out, errOut := runSyncForType(t, env, append([]string{"--type", typeDailyActivity}, window...)...)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	for _, want := range []string{
		"Synced daily activity from the Zepp cloud", "profile:", "123456",
		"range:", "2026-11-01 to 2026-11-30", "added:", "updated:", "skipped:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}

	// A repeat reports that nothing changed, naming the source.
	code, out, errOut = runSyncForType(t, env, append([]string{"--type", typeDailyActivity}, window...)...)
	if code != exitOK {
		t.Fatalf("repeat exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	if !strings.Contains(out, "No changes: all 1 daily activity rows from the Zepp cloud") {
		t.Errorf("repeat output does not report the no-change result:\n%s", out)
	}
}

// --- daily readback (offline, cloud mode) -----------------------------------

func TestDailyReadsCloudActivityOffline(t *testing.T) {
	stub := &activityStub{payload: []any{dayEntry("2026-11-19", daySummary(8000, 6100.5, 310.2))}}
	env := newCloudDailyEnv(t, stub)
	if code, _, out := runCloudDailySync(t, env); code != exitOK {
		t.Fatalf("sync exit = %d (output: %s)", code, out)
	}
	// From here on the command is read-only: no keyring, no network.
	forbidLocalAccess(t, &env)
	before := snapshot(t, env.databasePath)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-19", "--json")
	if code != exitOK {
		t.Fatalf("daily exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report dailyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.UserID != "123456" || len(report.Records) != 1 {
		t.Fatalf("report = %+v", report)
	}
	if report.Records[0].Steps != 8000 || report.Records[0].DistanceM != 6100.5 ||
		report.Records[0].ActiveKcal != 310.2 {
		t.Errorf("record = %+v, want the cloud values", report.Records[0])
	}
	if after := snapshot(t, env.databasePath); after != before {
		t.Fatalf("daily modified the database:\nbefore %q\nafter  %q", before, after)
	}
}

func TestDailyCloudModeIgnoresStaleExportPath(t *testing.T) {
	// The configuration still carries an export_path from an earlier source
	// switch: cloud mode must ignore it and resolve the stored profile.
	stub := &activityStub{payload: []any{dayEntry("2026-11-19", daySummary(8000, 6100.5, 310.2))}}
	env := newCloudDailyEnv(t, stub)
	writeConfigFile(t, env.configPath, `{
  "mode": "cloud_session",
  "timezone": "Europe/Madrid",
  "database_path": `+jsonString(t, env.databasePath)+`,
  "export_path": `+jsonString(t, filepath.Join(t.TempDir(), "export"))+`
}`)
	if code, _, out := runCloudDailySync(t, env); code != exitOK {
		t.Fatalf("sync exit = %d (output: %s)", code, out)
	}
	forbidLocalAccess(t, &env)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-19", "--json")
	if code != exitOK {
		t.Fatalf("daily exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report dailyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.UserID != "123456" {
		t.Errorf("user_id = %q, want the stored cloud profile, not the export hash", report.UserID)
	}
	if strings.HasPrefix(report.UserID, "export_") {
		t.Errorf("the stale export_path was used: %q", report.UserID)
	}
}

func TestDailyCloudProfileAmbiguityIsReported(t *testing.T) {
	stub := &activityStub{payload: []any{dayEntry("2026-11-19", daySummary(8000, 6100.5, 310.2))}}
	env := newCloudDailyEnv(t, stub)
	writeConfigFile(t, env.configPath, `{
  "mode": "cloud_session",
  "timezone": "Europe/Madrid",
  "database_path": `+jsonString(t, env.databasePath)+`,
  "export_path": `+jsonString(t, filepath.Join(t.TempDir(), "export"))+`
}`)
	if code, _, out := runCloudDailySync(t, env); code != exitOK {
		t.Fatalf("sync exit = %d (output: %s)", code, out)
	}
	// A second cloud profile makes the choice ambiguous. An export row would
	// not: cloud mode only ever looks at cloud-origin rows.
	db, err := openReadWrite(env.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO daily_activity (
	id, provider, source_type, user_id, timezone, date, steps, distance_m, active_kcal
) VALUES ('other', 'zepp_life', 'cloud_session', 'another-user', 'UTC', '2026-11-19', 5, 5, 5)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	forbidLocalAccess(t, &env)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-19", "--json")
	if code != exitError {
		t.Fatalf("daily exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report dailyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "several") {
		t.Fatalf("report = %+v, want the ambiguity reported", report)
	}
	// Cloud mode must not send the user back to the export setup.
	if strings.Contains(report.Detail, "--export-dir") {
		t.Errorf("detail = %q, do not suggest the export source in cloud mode", report.Detail)
	}
}

func TestDailyCloudModeIgnoresExportRows(t *testing.T) {
	// A cloud configuration over a database that also holds export rows: one
	// for a foreign profile (which would make the profile ambiguous) and one
	// for the cloud profile itself on the same date (which would be returned
	// as a cloud result). Neither may be read.
	stub := &activityStub{payload: []any{dayEntry("2026-11-19", daySummary(8000, 6100.5, 310.2))}}
	env := newCloudDailyEnv(t, stub)
	if code, _, out := runCloudDailySync(t, env); code != exitOK {
		t.Fatalf("sync exit = %d (output: %s)", code, out)
	}
	db, err := openReadWrite(env.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		id, userID, deviceKey string
		steps                 int64
	}{
		{id: "export-other", userID: "another-user", deviceKey: "", steps: 5},
		{id: "export-same-user", userID: "123456", deviceKey: "exported", steps: 99999},
	} {
		_, err = db.Exec(`
INSERT INTO daily_activity (
	id, provider, source_type, user_id, timezone, date, steps, distance_m, active_kcal, device_key
) VALUES (?, 'zepp_life', 'export_file', ?, 'UTC', '2026-11-19', ?, ?, ?, ?)`,
			row.id, row.userID, row.steps, float64(row.steps), float64(row.steps), row.deviceKey)
		if err != nil {
			t.Fatalf("insert export row %s: %v", row.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	forbidLocalAccess(t, &env)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-19", "--json")
	if code != exitOK {
		t.Fatalf("daily exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report dailyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.UserID != "123456" {
		t.Fatalf("report = %+v, want the configured cloud profile", report)
	}
	if len(report.Records) != 1 || report.Records[0].Steps != 8000 {
		t.Errorf("records = %+v, want only the cloud row with 8000 steps", report.Records)
	}
}
