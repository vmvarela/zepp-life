package main

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// expectedSchemaVersion is the only database schema this build reads. A
// database in any other state is reported, never changed.
const expectedSchemaVersion = 6

const bodyTable = "body_measurements"
const activityTable = "daily_activity"

// openReadOnly opens the database with SQLite's mode=ro: the file is never
// created, no journal is ever written and no statement below is anything but
// a read.
func openReadOnly(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", readOnlyDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// readOnlyDSN builds a file: URI. The path is percent-encoded so that a '?' or
// '#' inside the file name cannot be taken for the query part of the URI:
// without that, mode=ro would be silently dropped and the name truncated.
func readOnlyDSN(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // Windows drive letter: file:///C:/...
	}
	return "file:" + escapeURIPath(p) + "?mode=ro"
}

func escapeURIPath(path string) string {
	var b strings.Builder
	for i := 0; i < len(path); i++ {
		c := path[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~', c == '/', c == ':':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// schemaVersion reads PRAGMA user_version through its pragma function so that
// every statement this program runs stays a SELECT.
func schemaVersion(db *sql.DB) (int, error) {
	var raw string
	if err := db.QueryRow("SELECT * FROM pragma_user_version").Scan(&raw); err != nil {
		return 0, err
	}
	version, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("unexpected schema version %q", raw)
	}
	return version, nil
}

// bodyTableExists reports whether the body measurement table is present.
func bodyTableExists(db *sql.DB) (bool, error) {
	return tableExists(db, bodyTable)
}

// tableExists reports whether the named table is present.
func tableExists(db *sql.DB, table string) (bool, error) {
	var name string
	err := db.QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = ? LIMIT 1",
		table,
	).Scan(&name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}

// distinctUserIDs returns up to limit distinct profiles stored in the body
// table. One result means the profile can be derived from the database alone.
func distinctUserIDs(db *sql.DB, limit int) ([]string, error) {
	return distinctIDsIn(db, bodyTable, "", limit)
}

// distinctActivityUserIDs returns up to limit distinct profiles stored in the
// daily activity table. A non-empty sourceType restricts the profiles to rows
// of that source, so rows imported from another source can neither create
// ambiguity nor be reported as belonging to it.
func distinctActivityUserIDs(db *sql.DB, sourceType string, limit int) ([]string, error) {
	return distinctIDsIn(db, activityTable, sourceType, limit)
}

// distinctIDsIn reads distinct user_id values from a fixed table name (never
// from data) with a caller-supplied bound. An empty sourceType reads every
// source; otherwise only rows of that source are considered.
func distinctIDsIn(db *sql.DB, table, sourceType string, limit int) ([]string, error) {
	query := "SELECT DISTINCT user_id FROM " + table
	args := []any{}
	if sourceType != "" {
		query += " WHERE source_type = ?"
		args = append(args, sourceType)
	}
	query += " LIMIT ?"
	args = append(args, limit)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// activityDayRow is one stored daily activity row as `daily` reads it.
type activityDayRow struct {
	deviceID   sql.NullString
	deviceKey  string
	steps      int64
	distanceM  float64
	activeKcal float64
}

// activityForDate returns all and only the rows of userID on date, in a
// stable order: device_key first (devices are reported separately), then id
// to break any tie deterministically. The bound is the primary key of the
// table, so at most one row per device can match. An empty sourceType reads
// rows of every source; otherwise only rows of that source are returned.
func activityForDate(db *sql.DB, userID, date, sourceType string) ([]activityDayRow, error) {
	query := `
SELECT device_id, device_key, steps, distance_m, active_kcal
FROM ` + activityTable + `
WHERE user_id = ? AND date = ?`
	args := []any{userID, date}
	if sourceType != "" {
		query += " AND source_type = ?"
		args = append(args, sourceType)
	}
	query += `
ORDER BY device_key, id`
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activityDayRow
	for rows.Next() {
		var row activityDayRow
		if err := rows.Scan(&row.deviceID, &row.deviceKey, &row.steps, &row.distanceM, &row.activeKcal); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// measurement is one body measurement row, with the field names the Python
// models use. Nullable columns stay null in JSON.
type measurement struct {
	ID                  string   `json:"id"`
	UserID              string   `json:"user_id"`
	DeviceID            *string  `json:"device_id"`
	Timezone            *string  `json:"timezone"`
	Timestamp           string   `json:"timestamp"`
	LocalDate           *string  `json:"local_date"`
	WeightKg            float64  `json:"weight_kg"`
	BMI                 *float64 `json:"bmi"`
	BodyFatPct          *float64 `json:"body_fat_pct"`
	MuscleMassKg        *float64 `json:"muscle_mass_kg"`
	WaterPct            *float64 `json:"water_pct"`
	BoneMassKg          *float64 `json:"bone_mass_kg"`
	VisceralFatScore    *int64   `json:"visceral_fat_score"`
	BasalMetabolismKcal *int64   `json:"basal_metabolism_kcal"`
	MetabolicAge        *int64   `json:"metabolic_age"`
}

// latestMeasurement returns at most one row: the newest stored timestamp for
// the profile. Timestamps are compared exactly as they are stored, so the
// stored order is the order reported; insertion order breaks exact ties.
func latestMeasurement(db *sql.DB, userID string) (*measurement, error) {
	var m measurement
	var deviceID, timezone, localDate sql.Null[string]
	var bmi, bodyFat, muscleMass, water, boneMass sql.Null[float64]
	var visceralFat, basalMetabolism, metabolicAge sql.Null[int64]

	err := db.QueryRow(`
SELECT id, user_id, device_id, timezone, timestamp, local_date, weight_kg, bmi,
       body_fat_pct, muscle_mass_kg, water_pct, bone_mass_kg, visceral_fat_score,
       basal_metabolism_kcal, metabolic_age
FROM `+bodyTable+`
WHERE user_id = ?
ORDER BY timestamp DESC, rowid DESC
LIMIT 1`, userID).Scan(
		&m.ID, &m.UserID, &deviceID, &timezone, &m.Timestamp, &localDate,
		&m.WeightKg, &bmi, &bodyFat, &muscleMass, &water, &boneMass,
		&visceralFat, &basalMetabolism, &metabolicAge,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}

	m.DeviceID = nullString(deviceID)
	m.Timezone = nullString(timezone)
	m.LocalDate = nullString(localDate)
	m.BMI = nullFloat(bmi)
	m.BodyFatPct = nullFloat(bodyFat)
	m.MuscleMassKg = nullFloat(muscleMass)
	m.WaterPct = nullFloat(water)
	m.BoneMassKg = nullFloat(boneMass)
	m.VisceralFatScore = nullInt(visceralFat)
	m.BasalMetabolismKcal = nullInt(basalMetabolism)
	m.MetabolicAge = nullInt(metabolicAge)
	return &m, nil
}

func nullString(v sql.Null[string]) *string {
	if !v.Valid {
		return nil
	}
	return &v.V
}

func nullFloat(v sql.Null[float64]) *float64 {
	if !v.Valid {
		return nil
	}
	return &v.V
}

func nullInt(v sql.Null[int64]) *int64 {
	if !v.Valid {
		return nil
	}
	return &v.V
}
