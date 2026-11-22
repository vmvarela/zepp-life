package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenOrCreateDatabaseCreatesTheCompleteV6Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	db, created, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Fatal("created = false, want true for a missing file")
	}
	defer db.Close()

	version, err := schemaVersion(db)
	if err != nil || version != expectedSchemaVersion {
		t.Fatalf("schemaVersion = %d, %v; want %d, nil", version, err, expectedSchemaVersion)
	}
	// The complete schema: all six tables and every index, not just the ones
	// the body sync writes.
	for _, table := range v6Tables {
		ok, err := objectExists(db, "table", table)
		if err != nil || !ok {
			t.Errorf("table %s missing (err = %v)", table, err)
		}
	}
	for _, index := range v6Indexes {
		ok, err := objectExists(db, "index", index)
		if err != nil || !ok {
			t.Errorf("index %s missing (err = %v)", index, err)
		}
	}
	if err := verifyV6Schema(db, path); err != nil {
		t.Fatalf("verifyV6Schema: %v", err)
	}

	// Both unique constraints of the body table exist: the original table
	// constraint and the later business-key index.
	var tableSQL, indexSQL string
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, bodyTable,
	).Scan(&tableSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tableSQL, "UNIQUE(user_id, timestamp, device_id)") {
		t.Errorf("body table lost its original unique constraint:\n%s", tableSQL)
	}
	if err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'idx_measurements_business_key'`,
	).Scan(&indexSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(indexSQL, "UNIQUE") {
		t.Errorf("business-key index is not unique:\n%s", indexSQL)
	}
}

func TestOpenOrCreateDatabaseReopeningChangesNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	db, _, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	before := snapshot(t, path)

	again, created, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	again.Close()
	if created {
		t.Fatal("created = true for an existing database")
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("reopening changed the database:\nbefore %q\nafter  %q", before, after)
	}
}

func TestOpenOrCreateDatabaseRejectsOtherSchemaVersions(t *testing.T) {
	for _, version := range []int{0, 5, 7} {
		path := newFixture(t, version, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
		before := snapshot(t, path)

		_, _, err := openOrCreateDatabase(path)
		if err == nil {
			t.Fatalf("version %d: error = nil, want a clear refusal", version)
		}
		if !strings.Contains(err.Error(), "not modified") || !strings.Contains(err.Error(), "version") {
			t.Errorf("version %d: error = %q", version, err)
		}
		if after := snapshot(t, path); after != before {
			t.Fatalf("version %d: database changed", version)
		}
	}
}

func TestOpenOrCreateDatabaseRejectsAnIncompleteV6Schema(t *testing.T) {
	// Version number right, content wrong: reported, never repaired.
	path := newFixture(t, 6, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	before := snapshot(t, path)

	_, _, err := openOrCreateDatabase(path)
	if err == nil {
		t.Fatal("error = nil, want a clear refusal")
	}
	if !strings.Contains(err.Error(), "complete version 6 schema") {
		t.Errorf("error = %q", err)
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("database changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestOpenOrCreateDatabaseCreatesMissingParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", databaseFile)
	db, created, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Close()
	if !created {
		t.Fatal("created = false")
	}
}

func TestOpenOrCreateDatabaseUsesPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes do not apply on Windows")
	}
	path := filepath.Join(t.TempDir(), "nested", databaseFile)
	db, created, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Close()
	if !created {
		t.Fatal("created = false")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("database mode = %o, want 600 (health data must stay private)", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory mode = %o, want 700 (private)", perm)
	}
}

func TestOpenOrCreateDatabaseKeepsTheModeOfAnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes do not apply on Windows")
	}
	path := filepath.Join(t.TempDir(), databaseFile)
	db, _, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	db.Close()
	// An existing installation may have its own mode: opening it for a sync
	// must never chmod it.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	again, _, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	again.Close()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode = %o, want 644 (an existing file must keep its mode)", perm)
	}
}

func TestOpenOrCreateDatabaseNeverRemovesAFileItDidNotCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), databaseFile)
	// O_EXCL reports the file as taken, so the verify path runs and refuses
	// it: the cleanup may only ever delete a file this call created.
	foreign := []byte("somebody else's data")
	if err := os.WriteFile(path, foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openOrCreateDatabase(path); err == nil {
		t.Fatal("error = nil, want a refusal")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("foreign file was removed: %v", err)
	}
	if string(got) != string(foreign) {
		t.Fatalf("foreign file changed to %q", got)
	}
}

func TestNewDatabaseMatchesTheFinalMigrationColumnOrder(t *testing.T) {
	// The migration chain appends columns: device_key (second schema change),
	// local_date (fourth) and the sleep metrics (fifth) land after the
	// baseline columns in that order. A new database must match an existing
	// one column for column.
	cases := map[string][]string{
		"daily_activity": {
			"id", "provider", "source_type", "source_record_id", "user_id",
			"device_id", "timezone", "collected_at", "created_at", "updated_at",
			"date", "steps", "distance_m", "active_kcal", "total_kcal", "floors",
			"active_minutes", "device_key",
		},
		"sleep_sessions": {
			"id", "provider", "source_type", "source_record_id", "user_id",
			"device_id", "timezone", "collected_at", "created_at", "updated_at",
			"sleep_id", "start_at", "end_at", "duration_minutes",
			"time_asleep_minutes", "time_awake_minutes", "sleep_score", "is_nap",
			"stages", "local_date", "rem_minutes", "wake_count",
		},
		"body_measurements": {
			"id", "provider", "source_type", "source_record_id", "user_id",
			"device_id", "timezone", "collected_at", "created_at", "updated_at",
			"timestamp", "weight_kg", "bmi", "body_fat_pct", "muscle_mass_kg",
			"water_pct", "bone_mass_kg", "visceral_fat_score",
			"basal_metabolism_kcal", "metabolic_age", "device_key", "local_date",
		},
	}

	path := filepath.Join(t.TempDir(), databaseFile)
	db, _, err := openOrCreateDatabase(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer db.Close()

	for table, want := range cases {
		got := tableColumnNames(t, db, table)
		if len(got) != len(want) {
			t.Errorf("%s columns = %v, want %v", table, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("%s column %d = %q, want %q\nfull order: %v",
					table, i, got[i], want[i], got)
			}
		}
	}
}

// tableColumnNames reads PRAGMA table_info in cid order.
func tableColumnNames(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query("SELECT name FROM pragma_table_info(?) ORDER BY cid", table)
	if err != nil {
		t.Fatalf("pragma %s: %v", table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return names
}
