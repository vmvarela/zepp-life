package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

// upsertOutcome partitions the records a sync stores, the same three results
// the Python storage layer reports.
type upsertOutcome int

const (
	outcomeInserted upsertOutcome = iota
	outcomeUpdated
	outcomeUnchanged
)

// storedMeasurement is the row already present on the business key. Only the
// columns that are compared live here: the primary key id is deliberately
// absent because an existing row keeps it.
type storedMeasurement struct {
	provider, sourceType     sql.NullString
	sourceRecordID, deviceID sql.NullString
	timezone, collectedAt    sql.NullString
	localDate                sql.NullString
	weight                   float64
	bmi, bodyFat             sql.NullFloat64
	muscle, water, bone      sql.NullFloat64
	visceral, metabolism     sql.NullInt64
	muscleAge                sql.NullInt64
}

// upsertBodyMeasurement stores one record on the business key
// (user_id, timestamp, device_key) with device_key = ”.
//
// An existing row keeps its id; only columns whose value really changed are
// written, and NULL compares equal to NULL, so a row that already matches is
// reported as unchanged and never updated again (which would otherwise bump
// updated_at on every run).
func upsertBodyMeasurement(ctx context.Context, tx *sql.Tx, r *weightRecord, timezone string) (upsertOutcome, error) {
	existing, found, err := selectStoredMeasurement(ctx, tx, r.userID, r.timestampText)
	if err != nil {
		return outcomeUnchanged, err
	}
	if !found {
		_, err := tx.ExecContext(ctx, `
INSERT INTO body_measurements (
	id, provider, source_type, source_record_id, user_id, device_id, device_key,
	timezone, collected_at, timestamp, local_date, weight_kg, bmi, body_fat_pct,
	muscle_mass_kg, water_pct, bone_mass_kg, visceral_fat_score,
	basal_metabolism_kcal, metabolic_age
) VALUES (?, ?, ?, NULL, ?, NULL, '', ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.id, providerZeppLife, sourceCloudSession, r.userID,
			timezone, r.timestampText, r.localDate,
			r.weight, r.bmi, r.bodyFatPct, r.muscleMassKg, r.waterPct, r.boneMassKg,
			r.visceralFat, r.metabolism, r.muscleAge,
		)
		if err != nil {
			return outcomeUnchanged, err
		}
		return outcomeInserted, nil
	}

	var (
		names  []string
		values []any
	)
	add := func(name string, changed bool, value any) {
		if changed {
			names = append(names, name)
			values = append(values, value)
		}
	}
	add("provider", existing.provider.String != providerZeppLife, providerZeppLife)
	add("source_type", existing.sourceType.String != sourceCloudSession, sourceCloudSession)
	add("source_record_id", existing.sourceRecordID.Valid, nil) // cloud rows have none
	add("device_id", existing.deviceID.Valid, nil)
	add("timezone", !sameText(existing.timezone, timezone), timezone)
	add("collected_at", existing.collectedAt.Valid, nil)
	add("local_date", !sameText(existing.localDate, r.localDate), r.localDate)
	add("weight_kg", existing.weight != r.weight, r.weight)
	add("bmi", !sameFloat(existing.bmi, r.bmi), r.bmi)
	add("body_fat_pct", !sameFloat(existing.bodyFat, r.bodyFatPct), r.bodyFatPct)
	add("muscle_mass_kg", !sameFloat(existing.muscle, r.muscleMassKg), r.muscleMassKg)
	add("water_pct", !sameFloat(existing.water, r.waterPct), r.waterPct)
	add("bone_mass_kg", !sameFloat(existing.bone, r.boneMassKg), r.boneMassKg)
	add("visceral_fat_score", !sameInt(existing.visceral, r.visceralFat), r.visceralFat)
	add("basal_metabolism_kcal", !sameInt(existing.metabolism, r.metabolism), r.metabolism)
	add("metabolic_age", !sameInt(existing.muscleAge, r.muscleAge), r.muscleAge)
	if len(names) == 0 {
		return outcomeUnchanged, nil
	}

	// Column names come from the literals above, never from data.
	query := "UPDATE body_measurements SET updated_at = CURRENT_TIMESTAMP"
	for _, name := range names {
		query += ", " + name + " = ?"
	}
	query += " WHERE user_id = ? AND timestamp = ? AND device_key = ''"
	if _, err := tx.ExecContext(ctx, query, append(values, r.userID, r.timestampText)...); err != nil {
		return outcomeUnchanged, err
	}
	return outcomeUpdated, nil
}

// selectStoredMeasurement loads the comparable columns of the row on the
// business key, if there is one.
func selectStoredMeasurement(ctx context.Context, tx *sql.Tx, userID, timestamp string) (storedMeasurement, bool, error) {
	var s storedMeasurement
	err := tx.QueryRowContext(ctx, `
SELECT provider, source_type, source_record_id, device_id, timezone,
       collected_at, local_date, weight_kg, bmi, body_fat_pct, muscle_mass_kg,
       water_pct, bone_mass_kg, visceral_fat_score, basal_metabolism_kcal,
       metabolic_age
FROM body_measurements
WHERE user_id = ? AND timestamp = ? AND device_key = ''`,
		userID, timestamp,
	).Scan(
		&s.provider, &s.sourceType, &s.sourceRecordID, &s.deviceID, &s.timezone,
		&s.collectedAt, &s.localDate, &s.weight, &s.bmi, &s.bodyFat, &s.muscle,
		&s.water, &s.bone, &s.visceral, &s.metabolism, &s.muscleAge,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedMeasurement{}, false, nil
	case err != nil:
		return storedMeasurement{}, false, err
	}
	return s, true, nil
}

// storedActivity is the row already present on the business key
// (user_id, date, device_key). Only the columns that are compared live here:
// the primary key id is deliberately absent because an existing row keeps it.
type storedActivity struct {
	provider, sourceType     sql.NullString
	sourceRecordID, deviceID sql.NullString
	timezone, collectedAt    sql.NullString
	steps                    int64
	distanceM, activeKcal    float64
	totalKcal                sql.NullFloat64
	floors, activeMinutes    sql.NullInt64
}

// activityTimezone is the timezone written on every activity row: neither
// the export folder nor a cloud day summary carries a zone, so both store
// UTC.
const activityTimezone = "UTC"

// activitySource is the stored origin of one activity row: the source_type
// written on insert and update, and the deterministic primary key a new row
// gets. The business key (user_id, date, device_key) still decides which
// row is addressed; an existing row keeps whatever id it already has.
type activitySource struct {
	sourceType string
	id         string
}

// exportActivitySource is the identity of a row imported from the export
// folder: export_file with the stable hash id.
func exportActivitySource(userID, date string) activitySource {
	return activitySource{sourceType: sourceExportFile, id: activityRowID(userID, date)}
}

// cloudActivitySource is the identity of a row fetched from the Zepp cloud:
// cloud_session with id=cloud_<uid>_<date>. The uid is digits-only (setup
// validates it) and the date is canonical, so the id cannot carry anything
// else.
func cloudActivitySource(userID, date string) activitySource {
	return activitySource{sourceType: sourceCloudSession, id: fmt.Sprintf("cloud_%s_%s", userID, date)}
}

// upsertDailyActivity stores one activity row on the business key
// (user_id, date, device_key) with device_key = ”, using source for the
// stored identity (id of a new row and source_type).
//
// An existing row keeps its id; only columns whose value really changed are
// written, and NULL compares equal to NULL, so a row that already matches is
// reported as unchanged and never updated again (which would otherwise bump
// updated_at on every run).
func upsertDailyActivity(ctx context.Context, tx *sql.Tx, userID string, r *activityRecord, source activitySource) (upsertOutcome, error) {
	existing, found, err := selectStoredActivity(ctx, tx, userID, r.date)
	if err != nil {
		return outcomeUnchanged, err
	}
	if !found {
		_, err := tx.ExecContext(ctx, `
INSERT INTO daily_activity (
	id, provider, source_type, source_record_id, user_id, device_id, timezone,
	collected_at, date, steps, distance_m, active_kcal, total_kcal, floors,
	active_minutes, device_key
) VALUES (?, ?, ?, NULL, ?, NULL, ?, NULL, ?, ?, ?, ?, NULL, NULL, NULL, '')`,
			source.id, providerZeppLife, source.sourceType, userID,
			activityTimezone, r.date, r.steps, r.distanceM, r.activeKcal,
		)
		if err != nil {
			return outcomeUnchanged, err
		}
		return outcomeInserted, nil
	}

	var (
		names  []string
		values []any
	)
	add := func(name string, changed bool, value any) {
		if changed {
			names = append(names, name)
			values = append(values, value)
		}
	}
	add("provider", existing.provider.String != providerZeppLife, providerZeppLife)
	add("source_type", existing.sourceType.String != source.sourceType, source.sourceType)
	add("source_record_id", existing.sourceRecordID.Valid, nil) // neither source has one
	add("device_id", existing.deviceID.Valid, nil)
	add("timezone", !sameText(existing.timezone, activityTimezone), activityTimezone)
	add("collected_at", existing.collectedAt.Valid, nil)
	add("steps", existing.steps != r.steps, r.steps)
	add("distance_m", existing.distanceM != r.distanceM, r.distanceM)
	add("active_kcal", existing.activeKcal != r.activeKcal, r.activeKcal)
	add("total_kcal", existing.totalKcal.Valid, nil) // neither source carries it
	add("floors", existing.floors.Valid, nil)
	add("active_minutes", existing.activeMinutes.Valid, nil)
	if len(names) == 0 {
		return outcomeUnchanged, nil
	}

	// Column names come from the literals above, never from data.
	query := "UPDATE daily_activity SET updated_at = CURRENT_TIMESTAMP"
	for _, name := range names {
		query += ", " + name + " = ?"
	}
	query += " WHERE user_id = ? AND date = ? AND device_key = ''"
	if _, err := tx.ExecContext(ctx, query, append(values, userID, r.date)...); err != nil {
		return outcomeUnchanged, err
	}
	return outcomeUpdated, nil
}

// activityRowID is the deterministic row id of an imported activity day:
// the same profile and date always produce the same id, so re-imports never
// invent new primary keys.
func activityRowID(userID, date string) string {
	sum := sha256.Sum256([]byte("zepp-life:activity:" + userID + ":" + date))
	return "activity_" + hex.EncodeToString(sum[:])
}

// selectStoredActivity loads the comparable columns of the row on the
// business key, if there is one.
func selectStoredActivity(ctx context.Context, tx *sql.Tx, userID, date string) (storedActivity, bool, error) {
	var s storedActivity
	err := tx.QueryRowContext(ctx, `
SELECT provider, source_type, source_record_id, device_id, timezone,
       collected_at, steps, distance_m, active_kcal, total_kcal, floors,
       active_minutes
FROM daily_activity
WHERE user_id = ? AND date = ? AND device_key = ''`,
		userID, date,
	).Scan(
		&s.provider, &s.sourceType, &s.sourceRecordID, &s.deviceID, &s.timezone,
		&s.collectedAt, &s.steps, &s.distanceM, &s.activeKcal, &s.totalKcal,
		&s.floors, &s.activeMinutes,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return storedActivity{}, false, nil
	case err != nil:
		return storedActivity{}, false, err
	}
	return s, true, nil
}

// sameText, sameFloat and sameInt compare stored NULLs consistently with the
// incoming values: NULL only equals an absent value, never a real one.
func sameText(stored sql.NullString, incoming string) bool {
	return stored.Valid && stored.String == incoming
}

func sameFloat(stored sql.NullFloat64, incoming *float64) bool {
	if incoming == nil {
		return !stored.Valid
	}
	return stored.Valid && stored.Float64 == *incoming
}

func sameInt(stored sql.NullInt64, incoming *int64) bool {
	if incoming == nil {
		return !stored.Valid
	}
	return stored.Valid && stored.Int64 == *incoming
}
