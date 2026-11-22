package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// v6Schema is the complete final schema the Python tool creates: all six
// tables and every index, in the shape they have at its latest schema version
// (including both the original table constraints and the later business-key
// indexes). Columns appear in the order the migration chain produces them:
// baseline columns first, then the columns appended by later changes
// (device_key, then local_date, then the sleep metrics) in that order, so a
// new database matches an existing one column for column. A new database is
// built from exactly these statements in one transaction, ending with
// user_version = 6, so both tools can share the file.
var v6Schema = []string{
	`CREATE TABLE IF NOT EXISTS daily_activity (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		source_type TEXT NOT NULL,
		source_record_id TEXT,
		user_id TEXT NOT NULL,
		device_id TEXT,
		timezone TEXT DEFAULT 'UTC',
		collected_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		date TEXT NOT NULL,
		steps INTEGER NOT NULL DEFAULT 0,
		distance_m REAL NOT NULL DEFAULT 0,
		active_kcal REAL NOT NULL DEFAULT 0,
		total_kcal REAL,
		floors INTEGER,
		active_minutes INTEGER,
		device_key TEXT NOT NULL DEFAULT '',
		UNIQUE(user_id, date, device_id)
	)`,
	`CREATE TABLE IF NOT EXISTS sleep_sessions (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		source_type TEXT NOT NULL,
		source_record_id TEXT,
		user_id TEXT NOT NULL,
		device_id TEXT,
		timezone TEXT DEFAULT 'UTC',
		collected_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		sleep_id TEXT NOT NULL,
		start_at TIMESTAMP NOT NULL,
		end_at TIMESTAMP NOT NULL,
		duration_minutes INTEGER NOT NULL,
		time_asleep_minutes INTEGER NOT NULL,
		time_awake_minutes INTEGER NOT NULL,
		sleep_score INTEGER,
		is_nap BOOLEAN DEFAULT FALSE,
		stages TEXT,
		local_date TEXT,
		rem_minutes INTEGER NOT NULL DEFAULT 0,
		wake_count INTEGER NOT NULL DEFAULT 0,
		UNIQUE(user_id, sleep_id)
	)`,
	`CREATE TABLE IF NOT EXISTS workouts (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		source_type TEXT NOT NULL,
		source_record_id TEXT,
		user_id TEXT NOT NULL,
		device_id TEXT,
		timezone TEXT DEFAULT 'UTC',
		collected_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		workout_id TEXT NOT NULL,
		activity_type TEXT NOT NULL,
		start_at TIMESTAMP NOT NULL,
		end_at TIMESTAMP NOT NULL,
		duration_minutes INTEGER NOT NULL,
		distance_m REAL,
		calories_kcal REAL,
		avg_heart_rate_bpm INTEGER,
		max_heart_rate_bpm INTEGER,
		avg_pace_sec_per_km REAL,
		max_pace_sec_per_km REAL,
		total_steps INTEGER,
		local_date TEXT,
		UNIQUE(user_id, workout_id)
	)`,
	`CREATE TABLE IF NOT EXISTS body_measurements (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		source_type TEXT NOT NULL,
		source_record_id TEXT,
		user_id TEXT NOT NULL,
		device_id TEXT,
		timezone TEXT DEFAULT 'UTC',
		collected_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		timestamp TIMESTAMP NOT NULL,
		weight_kg REAL NOT NULL,
		bmi REAL,
		body_fat_pct REAL,
		muscle_mass_kg REAL,
		water_pct REAL,
		bone_mass_kg REAL,
		visceral_fat_score INTEGER,
		basal_metabolism_kcal INTEGER,
		metabolic_age INTEGER,
		device_key TEXT NOT NULL DEFAULT '',
		local_date TEXT,
		UNIQUE(user_id, timestamp, device_id)
	)`,
	`CREATE TABLE IF NOT EXISTS heart_rate_samples (
		id TEXT PRIMARY KEY,
		provider TEXT NOT NULL,
		source_type TEXT NOT NULL,
		source_record_id TEXT,
		user_id TEXT NOT NULL,
		device_id TEXT,
		timezone TEXT DEFAULT 'UTC',
		collected_at TIMESTAMP,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		timestamp TIMESTAMP NOT NULL,
		bpm INTEGER NOT NULL,
		sample_type TEXT NOT NULL,
		local_date TEXT,
		UNIQUE(user_id, timestamp, sample_type)
	)`,
	`CREATE TABLE IF NOT EXISTS sync_state (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		source_type TEXT NOT NULL,
		user_id TEXT NOT NULL,
		data_type TEXT NOT NULL,
		last_attempt_at TIMESTAMP,
		last_success_at TIMESTAMP,
		cursor_date TEXT,
		records_count INTEGER DEFAULT 0,
		last_error TEXT,
		UNIQUE(source_type, user_id, data_type)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_activity_user_date ON daily_activity(user_id, date)`,
	`CREATE INDEX IF NOT EXISTS idx_sleep_user_start ON sleep_sessions(user_id, start_at)`,
	`CREATE INDEX IF NOT EXISTS idx_workouts_user_start ON workouts(user_id, start_at)`,
	`CREATE INDEX IF NOT EXISTS idx_measurements_user_ts ON body_measurements(user_id, timestamp)`,
	`CREATE INDEX IF NOT EXISTS idx_heart_rate_user_ts ON heart_rate_samples(user_id, timestamp)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_activity_business_key ON daily_activity(user_id, date, device_key)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_measurements_business_key ON body_measurements(user_id, timestamp, device_key)`,
	`CREATE INDEX IF NOT EXISTS idx_sleep_sessions_user_local_date ON sleep_sessions(user_id, local_date)`,
	`CREATE INDEX IF NOT EXISTS idx_workouts_user_local_date ON workouts(user_id, local_date)`,
	`CREATE INDEX IF NOT EXISTS idx_body_measurements_user_local_date ON body_measurements(user_id, local_date)`,
	`CREATE INDEX IF NOT EXISTS idx_heart_rate_samples_user_local_date ON heart_rate_samples(user_id, local_date)`,
}

// v6Tables and v6Indexes are the objects a database must already contain to
// be usable: anything else is reported and left untouched.
var v6Tables = []string{
	"daily_activity", "sleep_sessions", "workouts",
	"body_measurements", "heart_rate_samples", "sync_state",
}

var v6Indexes = []string{
	"idx_activity_user_date", "idx_sleep_user_start", "idx_workouts_user_start",
	"idx_measurements_user_ts", "idx_heart_rate_user_ts",
	"idx_activity_business_key", "idx_measurements_business_key",
	"idx_sleep_sessions_user_local_date", "idx_workouts_user_local_date",
	"idx_body_measurements_user_local_date", "idx_heart_rate_samples_user_local_date",
}

// openReadWrite opens the database for the sync writes. The caller must have
// checked that the file exists and holds the complete version 6 schema, or
// that it does not exist at all.
func openReadWrite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", readWriteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// openOrCreateDatabase returns a writable database with the complete version 6
// schema. A missing file is claimed with O_EXCL as a private 0600 file inside
// a private 0700 directory and built from the full schema; an existing file is
// only verified — never modified, never chmod'ed and never removed.
func openOrCreateDatabase(path string) (*sql.DB, bool, error) {
	// MkdirAll does not change the mode of directories that already exist,
	// so an existing installation keeps its own permissions.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, fmt.Errorf("cannot create the data directory: %w", err)
	}

	// O_EXCL settles the creation race: if anybody else already created the
	// file we take the verify path, and the only file this function may ever
	// delete is the one this very call created.
	claimed, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	created := false
	switch {
	case err == nil:
		created = true
		// Best effort against a hostile umask; a no-op on Windows, which has
		// no POSIX modes to set.
		_ = claimed.Chmod(0o600)
		_ = claimed.Close()
	case errors.Is(err, os.ErrExist):
		// Somebody else's file: verify it below and change nothing.
	default:
		return nil, false, fmt.Errorf("cannot create the database file: %w", err)
	}

	db, err := openReadWrite(path)
	if err != nil {
		removeClaimedDatabase(path, created)
		return nil, false, fmt.Errorf("cannot open the database: %w", err)
	}

	if !created {
		if err := verifyV6Schema(db, path); err != nil {
			db.Close()
			return nil, false, err
		}
		return db, false, nil
	}

	// From here on the file is one this call created with O_EXCL: every
	// failure path may remove it without risking state that already existed.
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		db.Close()
		removeClaimedDatabase(path, created)
		return nil, false, fmt.Errorf("cannot start creating the database: %w", err)
	}
	for _, statement := range v6Schema {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			tx.Rollback()
			db.Close()
			removeClaimedDatabase(path, created)
			return nil, false, fmt.Errorf("cannot create the database schema: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = 6"); err != nil {
		tx.Rollback()
		db.Close()
		removeClaimedDatabase(path, created)
		return nil, false, fmt.Errorf("cannot set the database schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		db.Close()
		removeClaimedDatabase(path, created)
		return nil, false, fmt.Errorf("cannot finish creating the database: %w", err)
	}
	if err := verifyV6Schema(db, path); err != nil {
		db.Close()
		removeClaimedDatabase(path, created)
		return nil, false, err
	}
	return db, true, nil
}

// removeClaimedDatabase deletes the database file only when this invocation
// created it; state that already existed is never touched.
func removeClaimedDatabase(path string, created bool) {
	if created {
		_ = os.Remove(path)
	}
}

// verifyV6Schema checks that an existing database holds the complete version 6
// schema. It never writes: a database that fails the check is reported and
// left exactly as it is.
func verifyV6Schema(db *sql.DB, path string) error {
	version, err := schemaVersion(db)
	if err != nil {
		return fmt.Errorf("cannot read the schema version of %s: %w", path, err)
	}
	if version != expectedSchemaVersion {
		return fmt.Errorf(
			"schema version %d cannot be used by this build (it needs version %d). The database was not modified",
			version, expectedSchemaVersion,
		)
	}

	var missing []string
	for _, table := range v6Tables {
		ok, err := objectExists(db, "table", table)
		if err != nil {
			return fmt.Errorf("cannot inspect %s: %w", path, err)
		}
		if !ok {
			missing = append(missing, "table "+table)
		}
	}
	for _, index := range v6Indexes {
		ok, err := objectExists(db, "index", index)
		if err != nil {
			return fmt.Errorf("cannot inspect %s: %w", path, err)
		}
		if !ok {
			missing = append(missing, "index "+index)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf(
			"%s does not contain the complete version 6 schema (missing %s). The database was not modified",
			path, strings.Join(missing, ", "),
		)
	}

	for _, required := range []struct {
		table   string
		columns []string
	}{
		{table: bodyTable, columns: []string{"device_key", "local_date"}},
		{table: "sync_state", columns: []string{"cursor_date", "last_error", "last_success_at"}},
	} {
		ok, err := tableHasColumns(db, required.table, required.columns...)
		if err != nil {
			return fmt.Errorf("cannot inspect %s: %w", path, err)
		}
		if !ok {
			return fmt.Errorf(
				"the %s table in %s is missing required columns. The database was not modified",
				required.table, path,
			)
		}
	}
	return nil
}

func objectExists(db *sql.DB, kind, name string) (bool, error) {
	var got string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = ? AND name = ? LIMIT 1",
		kind, name,
	).Scan(&got)
	switch {
	case err == sql.ErrNoRows:
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

func tableHasColumns(db *sql.DB, table string, columns ...string) (bool, error) {
	rows, err := db.Query("SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, column := range columns {
		if !present[column] {
			return false, nil
		}
	}
	return true, nil
}

// readWriteDSN builds the file: URI used for the (never secret) write path.
func readWriteDSN(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive letter: file:///C:/...
	}
	return "file:" + escapeURIPath(p)
}

// syncContext bounds one whole database section of a sync run.
func syncContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), requestTimeout)
}

// clockNow never lets a zero time escape: the injected clock must always
// provide a real instant.
func clockNow(env environment) time.Time {
	if env.now != nil {
		return env.now()
	}
	return time.Now()
}
