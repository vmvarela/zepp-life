package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// forbiddenTransport fails the test if any code path reaches for the network.
type forbiddenTransport struct{ t *testing.T }

func (f forbiddenTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Error("network request attempted")
	return nil, errors.New("the network is forbidden in this test")
}

// forbidLocalAccess replaces every local-only seam with one that fails the
// test: keyring read, keyring write, hidden prompt and HTTP client.
func forbidLocalAccess(t *testing.T, env *environment) {
	t.Helper()
	env.readSecret = func(string, string) (string, error) {
		t.Error("keyring read attempted")
		return "", errors.New("the keyring is forbidden in this test")
	}
	env.writeSecret = func(string, string, string) error {
		t.Error("keyring write attempted")
		return errors.New("the keyring is forbidden in this test")
	}
	env.promptSecret = func(string) (string, error) {
		t.Error("prompt attempted")
		return "", errors.New("the prompt is forbidden in this test")
	}
	env.httpClient = &http.Client{Transport: forbiddenTransport{t}}
}

// newExportDir writes the given files (slash-separated relative paths) into a
// fresh temporary folder and returns its absolute path.
func newExportDir(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create export subfolder: %v", err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write export file %s: %v", name, err)
		}
	}
	return dir
}

// writeExportConfig stores the configuration an export setup produces.
func writeExportConfig(t *testing.T, path, databasePath, exportDir string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"mode":          modeExportFile,
		"export_path":   exportDir,
		"timezone":      "Europe/Madrid",
		"database_path": databasePath,
	})
	if err != nil {
		t.Fatalf("marshal export config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write export config: %v", err)
	}
}

// expectedExportUserID is the profile id contract: export_ plus the SHA-256
// of the absolute export folder path.
func expectedExportUserID(exportDir string) string {
	sum := sha256.Sum256([]byte(exportDir))
	return "export_" + hex.EncodeToString(sum[:])
}

// activityTestCSV is a minimal recognized export: one day of activity.
const activityTestCSV = "date,steps,distance_m,calories\n2026-11-01,8000,5000,300\n"

func runDailyFor(t *testing.T, env environment, extraArgs ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"daily"}, extraArgs...)
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

func runSetupForSource(t *testing.T, env environment, extraArgs ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"setup"}, extraArgs...)
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

func runSyncForType(t *testing.T, env environment, extraArgs ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"sync"}, extraArgs...)
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

// storedActivityRow mirrors one daily_activity row as the tests read it back.
type storedActivityRow struct {
	id           string
	provider     string
	sourceType   string
	sourceRecord sql.NullString
	userID       string
	deviceID     sql.NullString
	timezone     string
	collectedAt  sql.NullString
	date         string
	steps        int64
	distanceM    float64
	activeKcal   float64
	totalKcal    sql.NullFloat64
	floors       sql.NullInt64
	activeMin    sql.NullInt64
	deviceKey    string
}

func readActivityRows(t *testing.T, dbPath string) []storedActivityRow {
	t.Helper()
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open database read-only: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`
SELECT id, provider, source_type, source_record_id, user_id, device_id, timezone,
       collected_at, date, steps, distance_m, active_kcal, total_kcal, floors,
       active_minutes, device_key
FROM daily_activity
ORDER BY date, device_key, id`)
	if err != nil {
		t.Fatalf("query daily_activity: %v", err)
	}
	defer rows.Close()
	var out []storedActivityRow
	for rows.Next() {
		var r storedActivityRow
		if err := rows.Scan(
			&r.id, &r.provider, &r.sourceType, &r.sourceRecord, &r.userID,
			&r.deviceID, &r.timezone, &r.collectedAt, &r.date, &r.steps,
			&r.distanceM, &r.activeKcal, &r.totalKcal, &r.floors, &r.activeMin,
			&r.deviceKey,
		); err != nil {
			t.Fatalf("scan daily_activity row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate daily_activity: %v", err)
	}
	return out
}

func countRows(t *testing.T, dbPath, table string) int {
	t.Helper()
	db, err := openReadOnly(dbPath)
	if err != nil {
		t.Fatalf("open database read-only: %v", err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// --- setup --source export-file -------------------------------------------

func TestSetupExportFileConfiguresFolderWithoutKeyringOrNetwork(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	// An existing configuration with fields this setup does not own.
	db := env.databasePath
	writeConfigFile(t, env.configPath, `{
  "mode": "not_configured",
  "region": "us",
  "timezone": "Europe/Madrid",
  "database_path": `+jsonString(t, db)+`,
  "export_path": null,
  "stale_after_minutes": 90
}`)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Source != srcExportFile {
		t.Fatalf("report = %+v", report)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if report.ExportPath != absDir {
		t.Errorf("export_path = %q, want %q", report.ExportPath, absDir)
	}

	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig() = %+v, %q, %v", cfg, state, err)
	}
	if cfg.mode != modeExportFile || cfg.exportPath != absDir {
		t.Fatalf("config = %+v, want mode %s and export_path %q", cfg, modeExportFile, absDir)
	}
	// Every field this setup does not own survives untouched.
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	for field, want := range map[string]string{
		"region":              `"us"`,
		"timezone":            `"Europe/Madrid"`,
		"stale_after_minutes": `90`,
		"database_path":       jsonString(t, db),
	} {
		if got := string(raw[field]); got != want {
			t.Errorf("%s = %s, want %s (preserved)", field, got, want)
		}
	}
}

func TestSetupExportFileOnCleanInstallCreatesTheConfiguration(t *testing.T) {
	dir := newExportDir(t, map[string]string{"steps/day.csv": activityTestCSV})
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig() = %+v, %q, %v", cfg, state, err)
	}
	if cfg.mode != modeExportFile || cfg.exportPath == "" || !filepath.IsAbs(cfg.exportPath) {
		t.Fatalf("config = %+v, want export_file with an absolute export_path", cfg)
	}
	if cfg.timezone != "UTC" || cfg.databasePath != env.databasePath {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
}

func TestSetupExportFileValidatesTheFolder(t *testing.T) {
	onlyUnrecognized := newExportDir(t, map[string]string{"notes.csv": "foo,bar\n1,2\n"})
	plainFile := filepath.Join(t.TempDir(), "a.csv")
	if err := os.WriteFile(plainFile, []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		dir  string
	}{
		{name: "missing folder", dir: filepath.Join(t.TempDir(), "nope")},
		{name: "no recognizable activity", dir: onlyUnrecognized},
		{name: "not a folder", dir: plainFile},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := newTestEnv(t)
			forbidLocalAccess(t, &env)
			writeConfigFile(t, env.configPath, `{"mode": "cloud_session", "timezone": "UTC"}`)
			before := snapshotConfig(t, env.configPath)

			code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", test.dir, "--json")
			if code != exitError {
				t.Fatalf("exit = %d, want %d (stdout: %s, stderr: %s)", code, exitError, out, errOut)
			}
			var report setupReport
			if err := json.Unmarshal([]byte(out), &report); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if report.Status != "error" || report.Detail == "" {
				t.Fatalf("report = %+v", report)
			}
			if after := snapshotConfig(t, env.configPath); after != before {
				t.Fatalf("config changed:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

func TestSetupExportFlagCombinationsAreRejected(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	for _, extra := range [][]string{
		{"--source", srcExportFile},                         // no --export-dir
		{"--export-dir", dir},                               // export-dir without export source
		{"--source", "carrier-pigeon", "--export-dir", dir}, // unknown source
		{"--source", srcExportFile, "--export-dir", dir, "--user-id", "123"},
		{"--source", srcExportFile, "--export-dir", dir, "--replace-token"},
	} {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		code, _, errOut := runSetupForSource(t, env, extra...)
		if code != exitUsage {
			t.Errorf("%v exit = %d, want %d", extra, code, exitUsage)
		}
		if !strings.Contains(errOut, "--") {
			t.Errorf("%v stderr does not explain the problem: %q", extra, errOut)
		}
		if configExists(t, env.configPath) {
			t.Errorf("%v wrote a configuration", extra)
		}
	}
}

func TestSetupLeavingExportFileRequiresAnExplicitCloudSource(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	// A platform-neutral absolute folder: loadConfig rejects a relative
	// export_path, and "/data/export" is not absolute on Windows.
	exportDir := filepath.Join(t.TempDir(), "export")
	original := `{"mode": "export_file", "export_path": ` + jsonString(t, exportDir) + `, "timezone": "UTC"}`
	writeConfigFile(t, env.configPath, original)

	// Without --source the export configuration is never left.
	code, out, errOut := runSetupForSource(t, env, "--json")
	if code != exitError {
		t.Fatalf("default exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, modeExportFile) {
		t.Fatalf("report = %+v", report)
	}
	if got := snapshotConfig(t, env.configPath); got != original {
		t.Fatalf("config changed to %q", got)
	}

	// The explicit source switches to the cloud and keeps every other field.
	code, out, errOut = runSetupForSource(t, env, "--source", srcCloudSession, "--json")
	if code != exitOK {
		t.Fatalf("explicit exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig() = %+v, %q, %v", cfg, state, err)
	}
	if cfg.mode != modeCloudSession {
		t.Fatalf("mode = %q, want %q", cfg.mode, modeCloudSession)
	}
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if got := string(raw["export_path"]); got != jsonString(t, exportDir) {
		t.Errorf("export_path = %s, want it preserved", got)
	}
}

func TestSetupExportFileDoesNotChangeTheCloudCredentials(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	writeValidConfig(t, env.configPath, env.databasePath)

	if code, _, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json"); code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if len(secrets) != 2 || secrets[keyringTokenAccount] != "synthetic-token" {
		t.Fatalf("cloud credentials changed: %v", secrets)
	}
	cfg, _, err := loadConfig(env.configPath, env.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.mode != modeExportFile {
		t.Fatalf("mode = %q, want %q", cfg.mode, modeExportFile)
	}
}

// --- discovery --------------------------------------------------------------

func TestDiscoverActivityCSVsAllowedLocationsAndOrder(t *testing.T) {
	dir := newExportDir(t, map[string]string{
		"ACTIVITY/x.csv":        activityTestCSV,
		"STEPS/y.csv":           activityTestCSV,
		"my_activity.csv":       activityTestCSV,
		"notes.csv":             "foo,bar\n1,2\n",
		"ACTIVITY/nested/z.csv": activityTestCSV, // never entered
		"sleep/s.csv":           activityTestCSV, // not an activity folder
		"deep/a/b/c.csv":        activityTestCSV, // never entered
	})

	files, err := discoverActivityCSVs(dir)
	if err != nil {
		t.Fatalf("discoverActivityCSVs() error = %v", err)
	}
	want := map[string]bool{
		filepath.Join(dir, "ACTIVITY", "x.csv"): true,
		filepath.Join(dir, "STEPS", "y.csv"):    true,
		filepath.Join(dir, "my_activity.csv"):   true,
		filepath.Join(dir, "notes.csv"):         true,
	}
	got := map[string]bool{}
	for _, path := range files {
		got[path] = true
	}
	if len(got) != len(want) {
		t.Fatalf("discovered %v, want exactly %v", files, want)
	}
	for path := range want {
		if !got[path] {
			t.Errorf("missing %s in %v", path, files)
		}
	}
	if !sort.StringsAreSorted(files) {
		t.Errorf("not sorted deterministically: %v", files)
	}
	for _, path := range files {
		if strings.Contains(path, "nested") || strings.Contains(path, "sleep") || strings.Contains(path, "deep") {
			t.Errorf("non-recursive rule broken: %s", path)
		}
	}
}

// The same file reached through two names (hard links, or ACTIVITY/ and
// activity/ on a case-insensitive filesystem) is imported once: candidates
// are deduplicated by file identity, not by spelling.
func TestDiscoverActivityCSVsKeepsOnePathPerFile(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.csv")
	second := filepath.Join(dir, "b.csv")
	if err := os.WriteFile(first, []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(first, second); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	files, err := discoverActivityCSVs(dir)
	if err != nil {
		t.Fatalf("discoverActivityCSVs() error = %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("discovered %v, want exactly one path for the two names", files)
	}
}

// --- parser -----------------------------------------------------------------

func TestParseActivityFileNormalizesDatesAliasesAndUnits(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    []activityRecord
	}{
		{
			name:    "utf8 bom and mixed case aliases",
			content: "\ufeffDate,STEP_COUNT,Distance_M,Energy\n2026-11-01,8000,5000,300\n",
			want:    []activityRecord{{date: "2026-11-01", steps: 8000, distanceM: 5000, activeKcal: 300}},
		},
		{
			name:    "slash date",
			content: "day,steps\n2026/11/02,10\n",
			want:    []activityRecord{{date: "2026-11-02", steps: 10}},
		},
		{
			name:    "dotted date",
			content: "date,steps\n03.11.2026,10\n",
			want:    []activityRecord{{date: "2026-11-03", steps: 10}},
		},
		{
			name:    "day first",
			content: "date,steps\n04/11/2026,10\n",
			want:    []activityRecord{{date: "2026-11-04", steps: 10}},
		},
		{
			name:    "day first wins over month first",
			content: "date,steps\n05/11/2026,10\n",
			want:    []activityRecord{{date: "2026-11-05", steps: 10}},
		},
		{
			name:    "month first when day first is impossible",
			content: "date,steps\n05/13/2026,10\n",
			want:    []activityRecord{{date: "2026-05-13", steps: 10}},
		},
		{
			name:    "datetime with space",
			content: "timestamp,steps\n2026-11-06 07:08:09,10\n",
			want:    []activityRecord{{date: "2026-11-06", steps: 10}},
		},
		{
			name:    "iso t",
			content: "time,steps\n2026-11-07T07:08:09,10\n",
			want:    []activityRecord{{date: "2026-11-07", steps: 10}},
		},
		{
			name:    "iso z",
			content: "date,steps\n2026-11-08T07:08:09Z,10\n",
			want:    []activityRecord{{date: "2026-11-08", steps: 10}},
		},
		{
			name:    "metres alias is literal",
			content: "date,distance_m\n2026-11-01,500\n",
			want:    []activityRecord{{date: "2026-11-01", distanceM: 500}},
		},
		{
			name:    "ambiguous distance below 100 reads as kilometres",
			content: "date,distance\n2026-11-01,50\n",
			want:    []activityRecord{{date: "2026-11-01", distanceM: 50000}},
		},
		{
			name:    "ambiguous distance at or above 100 reads as metres",
			content: "date,dist\n2026-11-01,150\n",
			want:    []activityRecord{{date: "2026-11-01", distanceM: 150}},
		},
		{
			name:    "steps truncate towards zero",
			content: "date,steps\n2026-11-01,123.9\n",
			want:    []activityRecord{{date: "2026-11-01", steps: 123}},
		},
		{
			name:    "empty metric cells stay zero",
			content: "date,steps,distance,calories\n2026-11-01,,,\n",
			want:    []activityRecord{{date: "2026-11-01"}},
		},
		{
			name:    "crlf line endings",
			content: "date,steps\r\n2026-11-01,42\r\n",
			want:    []activityRecord{{date: "2026-11-01", steps: 42}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "day.csv")
			if err := os.WriteFile(path, []byte(test.content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, recognized, err := parseActivityFile(path, &importBudget{})
			if err != nil {
				t.Fatalf("parseActivityFile() error = %v", err)
			}
			if !recognized {
				t.Fatal("header was not recognized")
			}
			if len(got) != len(test.want) {
				t.Fatalf("records = %+v, want %+v", got, test.want)
			}
			for i, want := range test.want {
				g := got[i]
				if g.date != want.date || g.steps != want.steps ||
					g.distanceM != want.distanceM || g.activeKcal != want.activeKcal {
					t.Errorf("record %d = %+v, want %+v", i, g, want)
				}
			}
		})
	}
}

func TestParseActivityFileIgnoresUnrecognizedCSVs(t *testing.T) {
	for name, content := range map[string]string{
		"unrelated header": "foo,bar\n1,2\n",
		"date only":        "date,note\n2026-11-01,hello\n",
		"empty file":       "",
		"json":             `{"date": "2026-11-01"}`,
		"malformed header": "date,\"steps\n2026-11-01,10\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.csv")
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			got, recognized, err := parseActivityFile(path, &importBudget{})
			if err != nil {
				t.Fatalf("error = %v, want none for an ignored file", err)
			}
			if recognized || len(got) != 0 {
				t.Fatalf("records = %+v recognized = %v, want ignored", got, recognized)
			}
		})
	}
}

func TestSetupExportFileRejectsAnUnreadableCandidate(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	large := filepath.Join(dir, "unrelated.csv")
	if err := os.WriteFile(large, make([]byte, maxExportFileBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	if configExists(t, env.configPath) {
		t.Fatal("setup wrote a configuration although a candidate file cannot be imported")
	}
}

func TestParseActivityFileReportsMalformedRows(t *testing.T) {
	tests := []struct {
		name       string
		header     string
		row        string
		wantColumn string
		leak       string // a value that must never appear in the error
	}{
		{name: "bad date", header: "date,steps", row: "banana,10", wantColumn: "date"},
		{name: "empty date", header: "date,steps", row: ",10", wantColumn: "date"},
		{name: "impossible date", header: "date,steps", row: "2026-02-30,10", wantColumn: "date"},
		{name: "bad number", header: "date,steps", row: "2026-11-01,notanumber42", wantColumn: "steps", leak: "notanumber42"},
		{name: "negative steps", header: "date,steps", row: "2026-11-01,-5", wantColumn: "steps"},
		{name: "absurd steps", header: "date,steps", row: "2026-11-01,1e30", wantColumn: "steps"},
		{name: "nan distance", header: "date,distance", row: "2026-11-01,NaN", wantColumn: "distance"},
		{name: "inf calories", header: "date,calories", row: "2026-11-01,Inf", wantColumn: "calories"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "day.csv")
			content := test.header + "\n" + test.row + "\n"
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			_, recognized, err := parseActivityFile(path, &importBudget{})
			if err == nil {
				t.Fatal("error = nil, want a malformed-row report")
			}
			if !recognized {
				t.Fatal("the header should be recognized so the row is parsed")
			}
			msg := err.Error()
			if !strings.Contains(msg, path) || !strings.Contains(msg, "row 2") {
				t.Errorf("error %q does not name the file and row", msg)
			}
			if !strings.Contains(msg, test.wantColumn) {
				t.Errorf("error %q does not name column %q", msg, test.wantColumn)
			}
			if test.leak != "" && strings.Contains(msg, test.leak) {
				t.Errorf("error %q leaks a cell value", msg)
			}
		})
	}
}

// --- sync --type daily_activity -------------------------------------------

// newExportSyncEnv builds an export configuration with failing local seams.
func newExportSyncEnv(t *testing.T, files map[string]string) (environment, string) {
	t.Helper()
	dir := newExportDir(t, files)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	return env, dir
}

func TestSyncDailyActivityImportsOfflineAndStoresContractFields(t *testing.T) {
	env, dir := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/one.csv": "Date,Step_Count,Distance_M,Energy\n2026-11-01,8000,5000,300\n2026-11-02,9000,6000,350\n",
		"root_steps.csv":   "day,steps\n2026-11-03,1000\n",
		"notes.csv":        "foo,bar\n1,2\n",
	})

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Type != typeDailyActivity {
		t.Fatalf("report = %+v", report)
	}
	if report.Added != 3 || report.Updated != 0 || report.Skipped != 0 {
		t.Errorf("counters = %d/%d/%d, want 3/0/0", report.Added, report.Updated, report.Skipped)
	}
	absDir, _ := filepath.Abs(dir)
	wantUser := expectedExportUserID(absDir)
	if report.UserID != wantUser {
		t.Errorf("user_id = %q, want %q", report.UserID, wantUser)
	}

	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 3 {
		t.Fatalf("stored %d rows, want 3", len(rows))
	}
	for _, row := range rows {
		if row.provider != providerZeppLife || row.sourceType != sourceExportFile {
			t.Errorf("provider/source = %q/%q", row.provider, row.sourceType)
		}
		if row.userID != wantUser {
			t.Errorf("user_id = %q, want %q", row.userID, wantUser)
		}
		if row.deviceID.Valid || row.deviceKey != "" {
			t.Errorf("device = %v/%q, want NULL and empty key", row.deviceID, row.deviceKey)
		}
		if row.timezone != "UTC" {
			t.Errorf("timezone = %q, want UTC", row.timezone)
		}
		if row.sourceRecord.Valid || row.collectedAt.Valid {
			t.Errorf("source_record_id/collected_at = %v/%v, want NULL", row.sourceRecord, row.collectedAt)
		}
		if row.totalKcal.Valid || row.floors.Valid || row.activeMin.Valid {
			t.Errorf("total_kcal/floors/active_minutes = %v/%v/%v, want NULL", row.totalKcal, row.floors, row.activeMin)
		}
	}
	if got := countRows(t, env.databasePath, "sync_state"); got != 0 {
		t.Errorf("sync_state holds %d rows; the export import has no cursor", got)
	}
}

func TestSyncDailyActivityIsIdempotentAndPartitionsCounters(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/a.csv": "date,steps\n2026-11-01,100\n2026-11-02,200\n",
	})

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitOK {
		t.Fatalf("first exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var first syncReport
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatal(err)
	}
	if first.Added != 2 || first.Updated != 0 || first.Skipped != 0 {
		t.Fatalf("first counters = %d/%d/%d, want 2/0/0", first.Added, first.Updated, first.Skipped)
	}
	ids := map[string]string{}
	for _, row := range readActivityRows(t, env.databasePath) {
		ids[row.date] = row.id
	}

	code, out, errOut = runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitOK {
		t.Fatalf("second exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var second syncReport
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatal(err)
	}
	if second.Added != 0 || second.Updated != 0 || second.Skipped != 2 {
		t.Fatalf("second counters = %d/%d/%d, want 0/0/2", second.Added, second.Updated, second.Skipped)
	}
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2", len(rows))
	}
	for _, row := range rows {
		if ids[row.date] != row.id {
			t.Errorf("id of %s changed: %q -> %q", row.date, ids[row.date], row.id)
		}
	}
}

func TestSyncDailyActivitySuggestsTheLatestImportedDay(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/a.csv": "date,steps\n2026-11-02,200\n2026-11-01,100\n",
	})

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if !strings.Contains(out, "Next: zepp-life daily --date 2026-11-02") {
		t.Errorf("output does not suggest the latest imported day:\n%s", out)
	}
}

func TestSyncDailyActivityExplainsAnEmptyExport(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/a.csv": "date,steps\n",
	})

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if !strings.Contains(out, "No daily activity rows found") ||
		strings.Contains(out, "Next: zepp-life daily --date") {
		t.Errorf("empty import output is misleading:\n%s", out)
	}
}

func TestSyncDailyActivityRepeatedRowsSameDay(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/dup.csv": "date,steps\n2026-11-01,100\n2026-11-01,100\n",
	})

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Added != 1 || report.Updated != 0 || report.Skipped != 1 {
		t.Fatalf("counters = %d/%d/%d, want 1/0/1 (the two rows partition)", report.Added, report.Updated, report.Skipped)
	}
	rows := readActivityRows(t, env.databasePath)
	if len(rows) != 1 || rows[0].steps != 100 {
		t.Fatalf("rows = %+v, want one row with 100 steps", rows)
	}
	idAfterFirst := rows[0].id

	// A corrected value updates the row in place and keeps its primary key.
	writeExportCSV(t, env, "ACTIVITY/dup.csv", "date,steps\n2026-11-01,150\n")
	code, out, errOut = runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitOK {
		t.Fatalf("second exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Added != 0 || report.Updated != 1 || report.Skipped != 0 {
		t.Fatalf("counters = %d/%d/%d, want 0/1/0", report.Added, report.Updated, report.Skipped)
	}
	rows = readActivityRows(t, env.databasePath)
	if len(rows) != 1 || rows[0].steps != 150 {
		t.Fatalf("rows = %+v, want the corrected value", rows)
	}
	if rows[0].id != idAfterFirst {
		t.Errorf("id changed on update: %q -> %q", idAfterFirst, rows[0].id)
	}
}

// writeExportCSV replaces one file of a configured export folder.
func writeExportCSV(t *testing.T, env environment, name, content string) {
	t.Helper()
	cfg, _, err := loadConfig(env.configPath, env.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.exportPath, filepath.FromSlash(name)), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSyncDailyActivityMalformedRowLeavesDatabaseUntouched(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/ok.csv": activityTestCSV,
	})
	if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
		t.Fatalf("seed exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	before := snapshot(t, env.databasePath)
	rowsBefore := readActivityRows(t, env.databasePath)

	writeExportCSV(t, env, "ACTIVITY/bad.csv", "date,steps\n2026-11-04,notanumber42\n")
	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "bad.csv") ||
		!strings.Contains(report.Detail, "row 2") || !strings.Contains(report.Detail, "steps") {
		t.Fatalf("detail = %q, want file, row and column", report.Detail)
	}
	if strings.Contains(report.Detail, "notanumber42") {
		t.Errorf("detail leaks a cell value: %q", report.Detail)
	}
	if after := snapshot(t, env.databasePath); after != before {
		t.Fatalf("database changed after a failed import:\nbefore %q\nafter  %q", before, after)
	}
	if rows := readActivityRows(t, env.databasePath); len(rows) != len(rowsBefore) {
		t.Fatalf("row count changed: %d -> %d", len(rowsBefore), len(rows))
	}
}

func TestSyncDailyActivityMalformedRowDoesNotCreateADatabase(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/bad.csv": "date,steps\nbanana,10\n"})
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)

	code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	if _, err := os.Stat(env.databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database file exists after a failed import (stat err = %v)", err)
	}
}

func TestSyncDailyActivityRequiresRecognizableActivity(t *testing.T) {
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, newExportDir(t, map[string]string{
		"notes.csv": "foo,bar\n1,2\n",
	}))

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Detail, "does not look like an activity export") {
		t.Errorf("detail = %q, want it to explain the export requirement", report.Detail)
	}
}

func TestSyncRejectsInvalidSourceTypeCombinations(t *testing.T) {
	t.Run("daily_activity with a cloud session but no stored token", func(t *testing.T) {
		// The source follows the configuration: a cloud session routes this
		// to the Zepp cloud, which needs the token from setup. No request is
		// sent before the token check.
		secrets := map[string]string{}
		stub := &cloudStub{}
		env := newCloudSyncEnv(t, secrets, stub)
		code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
		}
		var report syncReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, "zepp-life setup") {
			t.Errorf("detail = %q, want the setup next step", report.Detail)
		}
		if hits := stub.hits(); len(hits) != 0 {
			t.Errorf("requests sent before the token check: %v", hits)
		}
	})

	t.Run("daily_activity without configuration", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
		}
		var report syncReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, srcExportFile) || !strings.Contains(report.Detail, "--export-dir") {
			t.Errorf("detail = %q, want the precise next step", report.Detail)
		}
	})

	t.Run("daily_activity with a vanished folder", func(t *testing.T) {
		env, exportDir := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
		if err := os.RemoveAll(exportDir); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
		}
		var report syncReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, "--export-dir") {
			t.Errorf("detail = %q, want the precise next step", report.Detail)
		}
	})

	t.Run("date flags do not apply to daily_activity", func(t *testing.T) {
		env, _ := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
		for _, extra := range [][]string{
			{"--type", typeDailyActivity, "--start-date", "2026-01-01"},
			{"--type", typeDailyActivity, "--end-date", "2026-01-01"},
		} {
			code, _, errOut := runSyncForType(t, env, extra...)
			if code != exitUsage {
				t.Errorf("%v exit = %d, want %d", extra, code, exitUsage)
			}
			if !strings.Contains(errOut, typeDailyActivity) {
				t.Errorf("stderr = %q, want it to name the type", errOut)
			}
		}
	})

	t.Run("unsupported type still names both supported types", func(t *testing.T) {
		env, _ := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
		code, _, errOut := runSyncForType(t, env, "--type", "sleep")
		if code != exitUsage {
			t.Fatalf("exit = %d, want %d", code, exitUsage)
		}
		if !strings.Contains(errOut, typeBodyMeasurements) || !strings.Contains(errOut, typeDailyActivity) {
			t.Errorf("stderr = %q, want both supported types", errOut)
		}
	})
}

// --- daily ------------------------------------------------------------------

func TestDailyReadsTheDatabaseAfterTheFolderIsGone(t *testing.T) {
	env, dir := newExportSyncEnv(t, map[string]string{
		"ACTIVITY/a.csv": "date,steps,distance,calories\n2026-11-01,8000,7.5,300\n",
	})
	if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
		t.Fatalf("sync exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	forbidLocalAccess(t, &env)
	before := snapshot(t, env.databasePath)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-01", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report dailyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Date != "2026-11-01" || len(report.Records) != 1 {
		t.Fatalf("report = %+v", report)
	}
	absDir, _ := filepath.Abs(dir)
	if report.UserID != expectedExportUserID(absDir) {
		t.Errorf("user_id = %q, want the deterministic export profile", report.UserID)
	}
	if report.Records[0].Steps != 8000 || report.Records[0].DistanceM != 7500 || report.Records[0].ActiveKcal != 300 {
		t.Errorf("record = %+v", report.Records[0])
	}
	if after := snapshot(t, env.databasePath); after != before {
		t.Fatalf("daily modified the database:\nbefore %q\nafter  %q", before, after)
	}
}

func TestDailyHumanOutputShowsDateProfileAndMetrics(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
		t.Fatalf("sync exit = %d (stderr: %s)", code, errOut)
	}
	forbidLocalAccess(t, &env)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-01")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	for _, want := range []string{
		"Daily activity 2026-11-01", "profile:", "steps:", "8000", "5000.0 m", "300.0 kcal",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
}

func TestDailySeparatesDevicesWithoutSumming(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
		t.Fatalf("sync exit = %d (stderr: %s)", code, errOut)
	}
	// A second device for the same day, written straight into the database.
	db, err := openReadWrite(env.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
INSERT INTO daily_activity (
	id, provider, source_type, user_id, device_id, timezone, date, steps,
	distance_m, active_kcal, device_key
) VALUES (?, ?, ?, ?, ?, 'UTC', ?, ?, ?, ?, ?)`,
		"activity_watch", providerZeppLife, sourceExportFile,
		expectedExportUserID(mustAbs(t, env)), "watch", "2026-11-01", 2222, 1000, 50, "watch")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	forbidLocalAccess(t, &env)

	code, out, errOut := runDailyFor(t, env, "--date", "2026-11-01", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report dailyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Records) != 2 {
		t.Fatalf("records = %+v, want one per device", report.Records)
	}
	var total int64
	for _, record := range report.Records {
		total += record.Steps
	}
	if total == 0 {
		t.Fatal("no steps reported")
	}
	if strings.Contains(out, "3333") {
		t.Errorf("human output sums devices:\n%s", out)
	}

	code, out, errOut = runDailyFor(t, env, "--date", "2026-11-01")
	if code != exitOK {
		t.Fatalf("human exit = %d (stderr: %s)", code, errOut)
	}
	if got := strings.Count(out, "device:"); got != 2 {
		t.Errorf("human output shows %d devices, want 2:\n%s", got, out)
	}
	if strings.Contains(out, "3333") {
		t.Errorf("human output sums devices:\n%s", out)
	}
}

// mustAbs returns the configured export folder of the environment.
func mustAbs(t *testing.T, env environment) string {
	t.Helper()
	cfg, _, err := loadConfig(env.configPath, env.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	return cfg.exportPath
}

func TestDailyExitCodesAndJSONShape(t *testing.T) {
	env, _ := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
		t.Fatalf("sync exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}

	t.Run("missing date is usage", func(t *testing.T) {
		if code, _, _ := runDailyFor(t, env); code != exitUsage {
			t.Errorf("exit = %d, want %d", code, exitUsage)
		}
	})
	t.Run("invalid date is usage", func(t *testing.T) {
		if code, _, errOut := runDailyFor(t, env, "--date", "01-11-2026"); code != exitUsage {
			t.Errorf("exit = %d, want %d (stderr: %s)", code, exitUsage, errOut)
		}
	})
	t.Run("unexpected argument is usage", func(t *testing.T) {
		if code, _, _ := runDailyFor(t, env, "--date", "2026-11-01", "extra"); code != exitUsage {
			t.Errorf("exit = %d, want %d", code, exitUsage)
		}
	})
	t.Run("no data for the date is exit 0", func(t *testing.T) {
		code, out, errOut := runDailyFor(t, env, "--date", "2026-01-01", "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
		}
		var report dailyReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "no_data" || len(report.Records) != 0 {
			t.Fatalf("report = %+v", report)
		}
	})
	t.Run("json has exactly the documented keys", func(t *testing.T) {
		_, out, _ := runDailyFor(t, env, "--date", "2026-11-01", "--json")
		raw := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(out), &raw); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"command", "status", "date", "user_id", "records", "detail"} {
			if _, ok := raw[key]; !ok {
				t.Errorf("report is missing %q: %s", key, out)
			}
		}
		if len(raw) != 6 {
			t.Errorf("report has %d keys, want exactly 6: %s", len(raw), out)
		}
		var records []map[string]json.RawMessage
		if err := json.Unmarshal(raw["records"], &records); err != nil || len(records) != 1 {
			t.Fatalf("records = %s (%v)", raw["records"], err)
		}
		for _, key := range []string{"device_id", "device_key", "steps", "distance_m", "active_kcal"} {
			if _, ok := records[0][key]; !ok {
				t.Errorf("record is missing %q", key)
			}
		}
	})
}

func TestDailyMissingDatabaseIsNoDataAndBrokenDatabaseIsError(t *testing.T) {
	t.Run("missing database", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeExportConfig(t, env.configPath, env.databasePath, newExportDir(t, map[string]string{"a.csv": activityTestCSV}))
		code, out, errOut := runDailyFor(t, env, "--date", "2026-11-01", "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
		}
		var report dailyReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "no_data" {
			t.Fatalf("report = %+v, want no_data", report)
		}
	})

	t.Run("wrong schema version", func(t *testing.T) {
		path := newFixture(t, 5, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		env.databasePath = path
		writeExportConfig(t, env.configPath, path, newExportDir(t, map[string]string{"a.csv": activityTestCSV}))
		if code, _, _ := runDailyFor(t, env, "--date", "2026-11-01", "--json"); code != exitError {
			t.Errorf("exit = %d, want %d", code, exitError)
		}
	})

	t.Run("missing activity table", func(t *testing.T) {
		path := newFixtureWithoutBodyTable(t, 6)
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		env.databasePath = path
		writeExportConfig(t, env.configPath, path, newExportDir(t, map[string]string{"a.csv": activityTestCSV}))
		if code, _, _ := runDailyFor(t, env, "--date", "2026-11-01", "--json"); code != exitError {
			t.Errorf("exit = %d, want %d", code, exitError)
		}
	})

	t.Run("malformed configuration", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeConfigFile(t, env.configPath, "{not json")
		if code, _, _ := runDailyFor(t, env, "--date", "2026-11-01", "--json"); code != exitError {
			t.Errorf("exit = %d, want %d", code, exitError)
		}
	})
}

func TestDailyProfileResolution(t *testing.T) {
	t.Run("export_path wins even with other profiles present", func(t *testing.T) {
		env, _ := newExportSyncEnv(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
		if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
			t.Fatalf("sync exit = %d (stderr: %s)", code, errOut)
		}
		// A foreign profile in the same table must not create ambiguity.
		db, err := openReadWrite(env.databasePath)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`
INSERT INTO daily_activity (
	id, provider, source_type, user_id, timezone, date, steps, distance_m, active_kcal
) VALUES ('foreign', 'zepp_life', 'export_file', 'other-user', 'UTC', '2026-11-01', 5, 5, 5)`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		forbidLocalAccess(t, &env)

		code, out, _ := runDailyFor(t, env, "--date", "2026-11-01", "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d", code, exitOK)
		}
		var report dailyReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.UserID != expectedExportUserID(mustAbs(t, env)) {
			t.Errorf("user_id = %q, want the export profile", report.UserID)
		}
		if len(report.Records) != 1 || report.Records[0].Steps == 5 {
			t.Errorf("records = %+v, want only the export profile's row", report.Records)
		}
	})

	t.Run("single stored profile is used without export_path", func(t *testing.T) {
		dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeExportConfig(t, env.configPath, env.databasePath, dir)
		if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
			t.Fatalf("sync exit = %d (stderr: %s)", code, errOut)
		}
		// Drop export_path: the single stored profile must still resolve.
		writeConfigFile(t, env.configPath, `{"mode": "export_file", "timezone": "UTC", "database_path": `+jsonString(t, env.databasePath)+`}`)

		code, out, errOut := runDailyFor(t, env, "--date", "2026-11-01", "--json")
		if code != exitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
		}
		var report dailyReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "ok" || report.UserID == "" {
			t.Fatalf("report = %+v", report)
		}
	})

	t.Run("several profiles without export_path are an error", func(t *testing.T) {
		dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeExportConfig(t, env.configPath, env.databasePath, dir)
		if code, _, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json"); code != exitOK {
			t.Fatalf("sync exit = %d (stderr: %s)", code, errOut)
		}
		db, err := openReadWrite(env.databasePath)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.Exec(`
INSERT INTO daily_activity (
	id, provider, source_type, user_id, timezone, date, steps, distance_m, active_kcal
) VALUES ('other', 'zepp_life', 'export_file', 'another-user', 'UTC', '2026-11-01', 5, 5, 5)`)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		writeConfigFile(t, env.configPath, `{"mode": "export_file", "timezone": "UTC", "database_path": `+jsonString(t, env.databasePath)+`}`)
		forbidLocalAccess(t, &env)

		code, out, errOut := runDailyFor(t, env, "--date", "2026-11-01", "--json")
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
		}
		var report dailyReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if report.Status != "error" || !strings.Contains(report.Detail, "--export-dir") {
			t.Errorf("report = %+v, want an actionable error", report)
		}
	})
}

// --- doctor and body in export mode ----------------------------------------

func TestDoctorExportModeSkipsTheKeyring(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)

	var stdout, stderr strings.Builder
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q: %+v", report.Status, report)
	}
	if report.Keyring.Status != "skipped" {
		t.Errorf("keyring.status = %q, want skipped", report.Keyring.Status)
	}
	if report.Keyring.HasToken || report.Keyring.HasUserID {
		t.Errorf("keyring = %+v, want nothing reported as stored", report.Keyring)
	}
	if report.Export.Status != "ok" || report.Export.Path == "" {
		t.Errorf("export = %+v, want ok with a path", report.Export)
	}
}

func TestDoctorExportFolderProblemsAreInformational(t *testing.T) {
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, filepath.Join(t.TempDir(), "gone"))

	var stdout, stderr strings.Builder
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(stdout.String()), &report); err != nil {
		t.Fatal(err)
	}
	if report.Export.Status != "not_found" {
		t.Errorf("export.status = %q, want not_found", report.Export.Status)
	}
	if report.Status != "ok" {
		t.Errorf("status = %q, want ok: a vanished folder is not a doctor failure", report.Status)
	}
}

func TestBodyExportModeNeverReadsTheKeyring(t *testing.T) {
	db := newFixture(t, 6, fixtureRow{id: "a", userID: "u42", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	dir := newExportDir(t, map[string]string{"ACTIVITY/a.csv": activityTestCSV})
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	env.databasePath = db
	writeExportConfig(t, env.configPath, db, dir)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.UserID != "u42" {
		t.Fatalf("report = %+v, want the profile resolved from the database", report)
	}
}
