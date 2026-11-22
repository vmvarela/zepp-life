package main

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestOpenReadOnlyRejectsWritesAndKeepsFileIntact(t *testing.T) {
	path := newFixture(t, 6, fixtureRow{id: "m1", userID: "u1", timestamp: "2026-11-01T07:00:00+00:00", weight: 70.5})
	before := snapshot(t, path)

	db, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer db.Close()

	if _, err := db.Exec("INSERT INTO body_measurements (id, user_id, timestamp, weight_kg) VALUES ('x', 'u1', 'now', 1)"); err == nil {
		t.Fatal("write on a read-only connection succeeded")
	}
	if _, err := db.Exec("CREATE TABLE surprise (id TEXT)"); err == nil {
		t.Fatal("DDL on a read-only connection succeeded")
	}
	if _, err := db.Exec("PRAGMA user_version = 99"); err == nil {
		t.Fatal("schema version change on a read-only connection succeeded")
	}

	version, err := schemaVersion(db)
	if err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != 6 {
		t.Fatalf("schema version = %d, want 6", version)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if after := snapshot(t, path); after != before {
		t.Fatalf("reading changed the database:\nbefore %q\nafter  %q", before, after)
	}
}

func TestOpenReadOnlyNeverCreatesAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, databaseFile)

	db, err := openReadOnly(path)
	if err == nil {
		db.Close()
		t.Fatal("opening a missing database succeeded")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("missing database was created: %v", statErr)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("directory is not empty anymore: %v", entries)
	}
}

func TestOpenReadOnlyHandlesSpacesAndPunctuationInPath(t *testing.T) {
	dir := t.TempDir()
	// The read-only URI must not let '?' or '#' truncate the file name, which
	// would silently disable mode=ro. '?' is not a legal file name character
	// on Windows, so the file carries '#' alone there; the '?' escaping is
	// asserted on the URI itself below, which needs no file at all.
	name := "my db#part1.db"
	if runtime.GOOS != "windows" {
		name = "my db?part#1.db"
	}
	path := filepath.Join(dir, name)
	db, err := sql.Open("sqlite", fixtureDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(bodyMeasurementsDDL); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, path)

	ro, err := openReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	var count int
	if err := ro.QueryRow("SELECT count(*) FROM body_measurements").Scan(&count); err != nil {
		t.Fatalf("read: %v", err)
	}
	ro.Close()
	if after := snapshot(t, path); after != before {
		t.Fatalf("reading changed the database:\nbefore %q\nafter  %q", before, after)
	}

	// The '?' case cannot be a file name on Windows, so it is asserted on the
	// URI itself: an unescaped '?' would start the query part and drop mode=ro.
	if got := readOnlyDSN(filepath.Join(dir, "my db?part1.db")); !strings.Contains(got, "my%20db%3Fpart1.db") {
		t.Errorf("read-only DSN does not escape '?': %q", got)
	}
}

func TestSchemaVersion(t *testing.T) {
	for _, version := range []int{0, 5, 6, 7} {
		path := newFixture(t, version)
		db, err := openReadOnly(path)
		if err != nil {
			t.Fatalf("version %d: open: %v", version, err)
		}
		got, err := schemaVersion(db)
		db.Close()
		if err != nil {
			t.Fatalf("version %d: read: %v", version, err)
		}
		if got != version {
			t.Fatalf("schemaVersion() = %d, want %d", got, version)
		}
	}
}

func TestBodyTableExists(t *testing.T) {
	path := newFixture(t, 6)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ok, err := bodyTableExists(db)
	if err != nil || !ok {
		t.Fatalf("bodyTableExists() = %v, %v; want true, nil", ok, err)
	}

	missing := newFixtureWithoutBodyTable(t, 6)
	other, err := openReadOnly(missing)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	ok, err = bodyTableExists(other)
	if err != nil || ok {
		t.Fatalf("bodyTableExists() = %v, %v; want false, nil", ok, err)
	}
}

func TestDistinctUserIDs(t *testing.T) {
	tests := []struct {
		name string
		rows []fixtureRow
		want []string
	}{
		{name: "none", want: nil},
		{
			name: "one",
			rows: []fixtureRow{
				{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00+00:00", weight: 70},
				{id: "b", userID: "u1", timestamp: "2026-11-02T07:00:00+00:00", weight: 71},
			},
			want: []string{"u1"},
		},
		{
			name: "several are capped by the limit",
			rows: []fixtureRow{
				{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00+00:00", weight: 70},
				{id: "b", userID: "u2", timestamp: "2026-11-01T07:00:00+00:00", weight: 71},
				{id: "c", userID: "u3", timestamp: "2026-11-01T07:00:00+00:00", weight: 72},
			},
			want: []string{"u1", "u2"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := newFixture(t, 6, test.rows...)
			db, err := openReadOnly(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			got, err := distinctUserIDs(db, 2)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(test.want) {
				t.Fatalf("distinctUserIDs() = %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Fatalf("distinctUserIDs() = %v, want %v", got, test.want)
				}
			}
		})
	}
}

func TestLatestMeasurementUsesStoredTimestampOrder(t *testing.T) {
	t.Run("Z sorts after +00:00 for the same instant", func(t *testing.T) {
		path := newFixture(t, 6,
			fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T06:00:00Z", weight: 69},
			fixtureRow{id: "b", userID: "u1", timestamp: "2026-11-01T07:00:00+00:00", weight: 70},
			fixtureRow{id: "c", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 71},
		)
		assertLatest(t, path, "u1", "c", 71)
	})

	t.Run("exact ties fall back to insertion order", func(t *testing.T) {
		// Two devices may legitimately record the same timestamp; the table's
		// unique key allows that, so the tie must still be broken deterministically.
		path := newFixture(t, 6,
			fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70, device: "scale-1"},
			fixtureRow{id: "b", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 71, device: "scale-2"},
		)
		assertLatest(t, path, "u1", "b", 71)
	})

	t.Run("only the requested profile is read", func(t *testing.T) {
		path := newFixture(t, 6,
			fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70},
			fixtureRow{id: "b", userID: "u2", timestamp: "2026-11-02T07:00:00Z", weight: 90},
		)
		assertLatest(t, path, "u1", "a", 70)
	})

	t.Run("no rows for the profile", func(t *testing.T) {
		path := newFixture(t, 6, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
		db, err := openReadOnly(path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		row, err := latestMeasurement(db, "nobody")
		if err != nil {
			t.Fatal(err)
		}
		if row != nil {
			t.Fatalf("latestMeasurement() = %+v, want nil", row)
		}
	})
}

func TestLatestMeasurementNullableColumns(t *testing.T) {
	fat := 18.5
	path := newFixture(t, 6,
		fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70},
		fixtureRow{id: "b", userID: "u1", timestamp: "2026-11-02T07:00:00Z", weight: 71, bodyFat: &fat},
	)
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	row, err := latestMeasurement(db, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatal("latestMeasurement() = nil")
	}
	if row.ID != "b" || row.UserID != "u1" || row.Timestamp != "2026-11-02T07:00:00Z" || row.WeightKg != 71 {
		t.Fatalf("unexpected row: %+v", row)
	}
	if row.BodyFatPct == nil || *row.BodyFatPct != fat {
		t.Fatalf("body_fat_pct = %v, want %v", row.BodyFatPct, fat)
	}
	if row.BMI != nil || row.MuscleMassKg != nil || row.DeviceID != nil {
		t.Fatalf("null columns should stay nil: %+v", row)
	}

	// The older row keeps its own (absent) optional values.
	older, err := latestMeasurement(db, "u1")
	if err != nil {
		t.Fatal(err)
	}
	if older.ID != "b" {
		t.Fatalf("unexpected row: %+v", older)
	}
}

func assertLatest(t *testing.T, path, userID, wantID string, wantWeight float64) {
	t.Helper()
	db, err := openReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	row, err := latestMeasurement(db, userID)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		t.Fatalf("latestMeasurement(%q) = nil", userID)
	}
	if row.ID != wantID || row.WeightKg != wantWeight {
		t.Fatalf("latestMeasurement(%q) = id %q weight %v, want id %q weight %v",
			userID, row.ID, row.WeightKg, wantID, wantWeight)
	}
}
