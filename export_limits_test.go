package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file covers the import limits of the export folder pipeline:
// regular files only, 8 MiB per file, 256 candidate files, 64 MiB read and
// 100k parsed rows per run. Every failure path must leave the configuration
// and the database untouched.

// writeSparseFile creates a file of exactly size bytes without writing the
// payload: the holes read back as zeros, which is enough to trip the byte
// limits without filling the disk.
func writeSparseFile(t *testing.T, path string, size int64) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := file.Truncate(size); err != nil {
		file.Close()
		t.Fatalf("truncate %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// assertSetupFailedWithoutWriting checks that a failed setup left an existing
// configuration byte for byte as it was.
func assertSetupFailedWithoutWriting(t *testing.T, env environment, before, detail string) {
	t.Helper()
	if after := snapshotConfig(t, env.configPath); after != before {
		t.Fatalf("config changed after a failed setup:\nbefore %q\nafter  %q", before, after)
	}
	if detail == "" {
		t.Fatal("the failure has no actionable detail")
	}
}

// assertSyncFailedWithoutCreatingTheDatabase checks that a failed import
// never created or modified the database file.
func assertSyncFailedWithoutCreatingTheDatabase(t *testing.T, env environment) {
	t.Helper()
	if _, err := os.Stat(env.databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database file exists after a failed import (stat err = %v)", err)
	}
}

func TestSetupExportRejectsSymlinkToRegularFile(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/real.csv": activityTestCSV})
	link := filepath.Join(dir, "ACTIVITY", "link.csv")
	if err := os.Symlink(filepath.Join(dir, "ACTIVITY", "real.csv"), link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeValidConfig(t, env.configPath, env.databasePath)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, link) ||
		!strings.Contains(report.Detail, "symlink") {
		t.Fatalf("detail = %q, want the path and the word symlink", report.Detail)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
}

func TestSyncDailyActivityRejectsSymlinkToRegularFile(t *testing.T) {
	dir := newExportDir(t, map[string]string{"ACTIVITY/real.csv": activityTestCSV})
	link := filepath.Join(dir, "ACTIVITY", "link.csv")
	if err := os.Symlink(filepath.Join(dir, "ACTIVITY", "real.csv"), link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "symlink") {
		t.Fatalf("detail = %q, want the symlink report", report.Detail)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// --- per-file and aggregate size -------------------------------------------

func oversizedExportDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "day.csv"), []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	// Exactly one byte over the per-file cap.
	writeSparseFile(t, filepath.Join(dir, "huge.csv"), maxExportFileBytes+1)
	return dir
}

func TestSetupExportRejectsOversizedFile(t *testing.T) {
	dir := oversizedExportDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeValidConfig(t, env.configPath, env.databasePath)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Detail, "huge.csv") || !strings.Contains(report.Detail, "larger than") {
		t.Fatalf("detail = %q, want the path and the per-file cap", report.Detail)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
}

func TestSyncDailyActivityRejectsOversizedFile(t *testing.T) {
	dir := oversizedExportDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Detail, "huge.csv") || !strings.Contains(report.Detail, "larger than") {
		t.Fatalf("detail = %q, want the path and the per-file cap", report.Detail)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// aggregateExportDir holds one more whole-size file than fits in the
// aggregate byte budget: every file is exactly the per-file cap, so eight of
// them reach the 64 MiB aggregate limit exactly and the ninth trips it.
func aggregateExportDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i < maxExportTotalBytes/maxExportFileBytes+1; i++ {
		writeSparseFile(t, filepath.Join(dir, fmt.Sprintf("part%02d.csv", i)), maxExportFileBytes)
	}
	return dir
}

func TestSetupExportAggregateReadLimit(t *testing.T) {
	dir := aggregateExportDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeValidConfig(t, env.configPath, env.databasePath)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	wantLimit := fmt.Sprintf("%d", maxExportTotalBytes)
	if !strings.Contains(report.Detail, "bytes") || !strings.Contains(report.Detail, wantLimit) {
		t.Fatalf("detail = %q, want the aggregate byte limit %s", report.Detail, wantLimit)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
}

func TestSyncDailyActivityAggregateReadLimit(t *testing.T) {
	dir := aggregateExportDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	wantLimit := fmt.Sprintf("%d", maxExportTotalBytes)
	if !strings.Contains(report.Detail, "bytes") || !strings.Contains(report.Detail, wantLimit) {
		t.Fatalf("detail = %q, want the aggregate byte limit %s", report.Detail, wantLimit)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// --- file count ------------------------------------------------------------

func tooManyCSVsDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for i := 0; i <= maxExportFiles; i++ {
		name := filepath.Join(dir, fmt.Sprintf("f%03d.csv", i))
		if err := os.WriteFile(name, []byte(activityTestCSV), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDiscoverActivityCSVsFileCountLimit(t *testing.T) {
	dir := tooManyCSVsDir(t)
	files, err := discoverActivityCSVs(dir)
	if err == nil {
		t.Fatalf("discovered %d files without hitting the %d limit", len(files), maxExportFiles)
	}
	if !errors.Is(err, errExportLimit) {
		t.Fatalf("error = %v, want an errExportLimit violation", err)
	}
	if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), fmt.Sprintf("%d", maxExportFiles)) {
		t.Errorf("error = %q, want the folder and the count limit", err)
	}
}

func TestSetupExportFileCountLimitFailsBeforeWritingConfig(t *testing.T) {
	dir := tooManyCSVsDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeValidConfig(t, env.configPath, env.databasePath)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
}

func TestSyncDailyActivityFileCountLimitFailsBeforeCreatingDatabase(t *testing.T) {
	dir := tooManyCSVsDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Detail, fmt.Sprintf("%d", maxExportFiles)) {
		t.Errorf("detail = %q, want the file count limit", report.Detail)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// --- record count ----------------------------------------------------------

// rowsBeyondLimit writes one recognized CSV with maxExportRecords+1 data
// rows: one more than the run may retain.
func rowsBeyondLimit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var body strings.Builder
	body.Grow((maxExportRecords + 1) * len("2026-11-01,1\n"))
	body.WriteString("date,steps\n")
	for i := 0; i <= maxExportRecords; i++ {
		body.WriteString("2026-11-01,1\n")
	}
	path := filepath.Join(dir, "many.csv")
	if err := os.WriteFile(path, []byte(body.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSyncDailyActivityRecordLimitFailsBeforeCreatingDatabase(t *testing.T) {
	dir := rowsBeyondLimit(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	wantLimit := fmt.Sprintf("%d", maxExportRecords)
	if !strings.Contains(report.Detail, "many.csv") ||
		!strings.Contains(report.Detail, "rows") || !strings.Contains(report.Detail, wantLimit) {
		t.Fatalf("detail = %q, want the path and the %s rows limit", report.Detail, wantLimit)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// --- budget units ----------------------------------------------------------

// endlessReader never returns EOF: it stands in for a file that would grow
// forever if the per-file cap were not enforced on the handle.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestCappedReaderBoundsEveryRead(t *testing.T) {
	const cap = 1024

	// A source of exactly the cap ends in a clean EOF: the +1 byte of
	// budget distinguishes "file ends here" from "file keeps going".
	exact := &cappedReader{
		source:    strings.NewReader(strings.Repeat("x", cap)),
		path:      "exact.csv",
		remaining: cap + 1,
		budget:    &importBudget{},
	}
	got, err := io.ReadAll(exact)
	if err != nil || len(got) != cap {
		t.Fatalf("exact-cap read = %d bytes, %v; want %d bytes, nil", len(got), err, cap)
	}

	// One byte more trips the per-file limit instead of being read.
	budget := &importBudget{}
	over := &cappedReader{
		source:    endlessReader{},
		path:      "endless.csv",
		remaining: cap + 1,
		budget:    budget,
	}
	got, err = io.ReadAll(over)
	if !errors.Is(err, errExportLimit) {
		t.Fatalf("err = %v, want errExportLimit", err)
	}
	if len(got) > cap+1 {
		t.Fatalf("read %d bytes, want at most %d (cap+1)", len(got), cap+1)
	}
	if budget.err == nil {
		t.Fatal("the violation was not recorded on the budget")
	}
	if !strings.Contains(budget.err.Error(), "endless.csv") {
		t.Errorf("budget error = %q, want the path", budget.err)
	}
}

func TestImportBudgetFailsFastOnEveryCounter(t *testing.T) {
	// Aggregate bytes: exactly the limit is allowed, one byte more fails.
	budget := &importBudget{}
	chunk := 1 << 20
	for i := 0; i < maxExportTotalBytes/chunk; i++ {
		if err := budget.accountBytes("f.csv", chunk); err != nil {
			t.Fatalf("unexpected violation after %d bytes: %v", (i+1)*chunk, err)
		}
	}
	if err := budget.accountBytes("f.csv", 1); !errors.Is(err, errExportLimit) {
		t.Fatalf("one byte over = %v, want errExportLimit", err)
	}
	// Fail fast: the first message sticks and no further counting happens.
	first := budget.err
	if err := budget.addRecord("f.csv"); err != first {
		t.Fatalf("later check = %v, want the first violation %v", err, first)
	}
	if budget.records != 0 {
		t.Fatalf("records counted after the budget broke: %d", budget.records)
	}

	// Records: every row up to the limit is accepted, the next one fails.
	records := &importBudget{}
	for i := 0; i < maxExportRecords; i++ {
		if err := records.addRecord("f.csv"); err != nil {
			t.Fatalf("unexpected violation at row %d: %v", i+1, err)
		}
	}
	if err := records.addRecord("f.csv"); !errors.Is(err, errExportLimit) {
		t.Fatalf("one row over = %v, want errExportLimit", err)
	}
}

// --- directory entries -----------------------------------------------------

// tooManyEntriesExportDir holds one recognizable CSV plus more unrelated
// .txt names than the listing may scan: without the entry limit this folder
// would import fine, so a failure can only come from the entry count.
func tooManyEntriesExportDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ACTIVITY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ACTIVITY", "ok.csv"), []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxExportDirEntries; i++ {
		name := filepath.Join(dir, fmt.Sprintf("note%04d.txt", i))
		if err := os.WriteFile(name, []byte("unrelated"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDiscoverActivityCSVsEntryCountLimit(t *testing.T) {
	dir := tooManyEntriesExportDir(t)
	files, err := discoverActivityCSVs(dir)
	if err == nil {
		t.Fatalf("scanned an unbounded directory: discovered %d files", len(files))
	}
	if !errors.Is(err, errExportLimit) {
		t.Fatalf("error = %v, want an errExportLimit violation", err)
	}
	want := fmt.Sprintf("%d", maxExportDirEntries)
	if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), want) ||
		!strings.Contains(err.Error(), "entries") {
		t.Errorf("error = %q, want the folder, the entry count and the limit", err)
	}
}

func TestSetupExportEntryLimitFailsBeforeWritingConfig(t *testing.T) {
	dir := tooManyEntriesExportDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeValidConfig(t, env.configPath, env.databasePath)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d", maxExportDirEntries)
	if !strings.Contains(report.Detail, want) || !strings.Contains(report.Detail, "entries") {
		t.Fatalf("detail = %q, want the entry limit %s", report.Detail, want)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
}

func TestSyncDailyActivityEntryLimitFailsBeforeCreatingDatabase(t *testing.T) {
	dir := tooManyEntriesExportDir(t)
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	before := snapshotConfig(t, env.configPath)

	code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%d", maxExportDirEntries)
	if !strings.Contains(report.Detail, want) || !strings.Contains(report.Detail, "entries") {
		t.Fatalf("detail = %q, want the entry limit %s", report.Detail, want)
	}
	assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// --- fixed activity subfolders vs the export root ---------------------------

// The fixed activity folders are chosen by this tool, so a symlink under
// one of those names must not be traversed. The export root itself is the
// user's explicit choice and may be a symlink (covered below).
func TestSetupAndSyncRejectSymlinkedActivitySubdirectory(t *testing.T) {
	external := t.TempDir() // the tree the symlink would point at
	if err := os.WriteFile(filepath.Join(external, "data.csv"), []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// A valid root CSV: without the subfolder check this folder would
	// import, so the failure can only come from rejecting the symlink.
	if err := os.WriteFile(filepath.Join(dir, "day.csv"), []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "ACTIVITY")
	if err := os.Symlink(external, sub); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	t.Run("setup", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeValidConfig(t, env.configPath, env.databasePath)
		before := snapshotConfig(t, env.configPath)

		code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", dir, "--json")
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
		}
		var report setupReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, sub) || !strings.Contains(report.Detail, "symlink") {
			t.Fatalf("detail = %q, want the subfolder path and the symlink report", report.Detail)
		}
		assertSetupFailedWithoutWriting(t, env, before, report.Detail)
	})

	t.Run("sync", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeExportConfig(t, env.configPath, env.databasePath, dir)
		before := snapshotConfig(t, env.configPath)

		code, out, errOut := runSyncForType(t, env, "--type", typeDailyActivity, "--json")
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stderr: %s, stdout: %s)", code, exitError, errOut, out)
		}
		var report syncReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, "symlink") {
			t.Fatalf("detail = %q, want the symlink report", report.Detail)
		}
		// Nothing of the external tree was imported.
		assertSetupFailedWithoutWriting(t, env, before, report.Detail)
		assertSyncFailedWithoutCreatingTheDatabase(t, env)
	})
}

// The explicitly selected export root stays the user's choice: a symlinked
// root is followed end to end, unlike the fixed activity subfolders.
func TestExportRootMayBeASymlink(t *testing.T) {
	realDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(realDir, "ACTIVITY"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "ACTIVITY", "day.csv"), []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "export-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unavailable on this filesystem: %v", err)
	}

	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	code, out, errOut := runSetupForSource(t, env, "--source", srcExportFile, "--export-dir", link, "--json")
	if code != exitOK {
		t.Fatalf("setup exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig() = %+v, %q, %v", cfg, state, err)
	}
	absLink, err := filepath.Abs(link)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.exportPath != absLink {
		t.Errorf("export_path = %q, want the selected (unresolved) path %q", cfg.exportPath, absLink)
	}

	code, out, errOut = runSyncForType(t, env, "--type", typeDailyActivity, "--json")
	if code != exitOK {
		t.Fatalf("sync exit = %d, want %d (stderr: %s, stdout: %s)", code, exitOK, errOut, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.Status != "ok" || report.Added != 1 {
		t.Fatalf("report = %+v, want one imported row through the symlinked root", report)
	}
}
