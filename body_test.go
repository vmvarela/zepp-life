package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func runBodyFor(t *testing.T, env environment, extraArgs ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"body", "--latest-only"}, extraArgs...)
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

func TestBodyOnCleanInstallIsNormalAndExitsZero(t *testing.T) {
	env := newTestEnv(t)
	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}

	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "no_data" || report.Config != string(configMissing) {
		t.Fatalf("report = %+v", report)
	}
	if report.Measurement != nil {
		t.Fatalf("measurement = %+v, want null", report.Measurement)
	}
	if !strings.Contains(report.Detail, "normal on a clean install") {
		t.Errorf("detail = %q", report.Detail)
	}
}

func TestBodyWithoutConfigurationUsesTheDefaultDatabasePath(t *testing.T) {
	fat := 21.5
	db := newFixture(t, 6,
		fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70.5},
		fixtureRow{id: "b", userID: "u1", timestamp: "2026-11-02T07:00:00+00:00", weight: 71.5, bodyFat: &fat},
	)
	env := newTestEnv(t)
	env.databasePath = db

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Config != string(configMissing) {
		t.Fatalf("report = %+v", report)
	}
	if report.Measurement == nil {
		t.Fatal("measurement = nil")
	}
	// The stored timestamp decides, so "2026-11-02T07:00:00+00:00" wins.
	if report.Measurement.ID != "b" || report.Measurement.WeightKg != 71.5 {
		t.Fatalf("measurement = %+v", report.Measurement)
	}
	if report.Measurement.BodyFatPct == nil || *report.Measurement.BodyFatPct != fat {
		t.Fatalf("body_fat_pct = %v, want %v", report.Measurement.BodyFatPct, fat)
	}
	if report.DatabasePath != db {
		t.Errorf("database_path = %q, want %q", report.DatabasePath, db)
	}
}

func TestBodyPrefersTheStoredUserID(t *testing.T) {
	db := newFixture(t, 6,
		fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70},
		fixtureRow{id: "b", userID: "u2", timestamp: "2026-11-01T07:00:00Z", weight: 90},
	)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)
	env.readSecret = writeSecrets(map[string]string{keyringUserIDAccount: "u2"})

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.UserID != "u2" {
		t.Errorf("user_id = %q, want u2", report.UserID)
	}
	if report.Measurement == nil || report.Measurement.ID != "b" {
		t.Fatalf("measurement = %+v", report.Measurement)
	}
}

func TestBodyDerivesTheOnlyProfileFromTheDatabase(t *testing.T) {
	db := newFixture(t, 6,
		fixtureRow{id: "a", userID: "u42", timestamp: "2026-11-01T07:00:00Z", weight: 70},
		fixtureRow{id: "b", userID: "u42", timestamp: "2026-11-02T07:00:00Z", weight: 71},
	)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "ok" || report.UserID != "u42" {
		t.Fatalf("report = %+v", report)
	}
	if report.Measurement == nil || report.Measurement.ID != "b" {
		t.Fatalf("measurement = %+v", report.Measurement)
	}
}

func TestBodyAsksForAProfileWhenSeveralExist(t *testing.T) {
	db := newFixture(t, 6,
		fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70},
		fixtureRow{id: "b", userID: "u2", timestamp: "2026-11-01T07:00:00Z", weight: 90},
	)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "profile_required" || report.Measurement != nil {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Detail, "several user profiles") {
		t.Errorf("detail = %q", report.Detail)
	}
	// The profile selection command does not exist yet, so the message must
	// not advertise it as if it did.
	if strings.Contains(report.Detail, "setup") {
		t.Errorf("detail mentions a command that does not exist yet: %q", report.Detail)
	}
}

func TestBodyWithoutMeasurementsIsNormal(t *testing.T) {
	db := newFixture(t, 6)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "no_data" || report.Measurement != nil {
		t.Fatalf("report = %+v", report)
	}
}

func TestBodyReportsAMissingDatabaseAsNormal(t *testing.T) {
	env := newTestEnv(t)
	writeValidConfig(t, env.configPath, env.databasePath)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "no_data" || !strings.Contains(report.Detail, "No local database") {
		t.Fatalf("report = %+v", report)
	}
}

func TestBodyReportsAProfileWithoutMeasurements(t *testing.T) {
	db := newFixture(t, 6, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)
	env.readSecret = writeSecrets(map[string]string{keyringUserIDAccount: "u2"})

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "no_data" || report.UserID != "u2" {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Detail, "u2") {
		t.Errorf("detail = %q", report.Detail)
	}
}

func TestBodyFallsBackToTheDatabaseWhenTheKeychainFails(t *testing.T) {
	db := newFixture(t, 6, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)
	env.readSecret = func(service, account string) (string, error) {
		return "", errors.New("keychain is locked")
	}

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "ok" || report.UserID != "u1" {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Detail, "keychain is locked") {
		t.Errorf("detail = %q", report.Detail)
	}
}

func TestBodyKeychainFailureWithSeveralProfilesExitsOne(t *testing.T) {
	db := newFixture(t, 6,
		fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70},
		fixtureRow{id: "b", userID: "u2", timestamp: "2026-11-01T07:00:00Z", weight: 90},
	)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)
	env.readSecret = func(service, account string) (string, error) {
		return "", errors.New("keychain is locked")
	}

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "keychain is locked") {
		t.Fatalf("report = %+v", report)
	}
}

func TestBodyNeverModifiesAnUnsupportedDatabase(t *testing.T) {
	db := newFixture(t, 5, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70})
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)
	before := snapshot(t, db)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "not modified") {
		t.Fatalf("report = %+v", report)
	}
	if after := snapshot(t, db); after != before {
		t.Fatalf("body changed the database:\nbefore %q\nafter  %q", before, after)
	}
}

func TestBodyWithoutBodyTableExitsOne(t *testing.T) {
	db := newFixtureWithoutBodyTable(t, 6)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, bodyTable) {
		t.Fatalf("report = %+v", report)
	}
}

func TestBodyMalformedConfigurationExitsOne(t *testing.T) {
	env := newTestEnv(t)
	writeConfigFile(t, env.configPath, `{"mode": "cloud_session", "timezone": "Nowhere/Nothing"}`)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report bodyReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Status != "error" || report.Config != string(configInvalid) {
		t.Fatalf("report = %+v", report)
	}
}

func TestBodyHumanOutputShowsTheMeasurement(t *testing.T) {
	fat := 18.5
	db := newFixture(t, 6, fixtureRow{
		id: "a", userID: "u1", timestamp: "2026-11-02T07:00:00Z", weight: 71.5, bodyFat: &fat,
	})
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	code, out, errOut := runBodyFor(t, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	for _, want := range []string{
		"Latest body measurement",
		"u1",
		"2026-11-02T07:00:00Z",
		"71.5 kg",
		"18.5 %",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "bmi") {
		t.Errorf("absent optional values should not be printed:\n%s", out)
	}
}

func TestBodyJSONKeepsNullableColumnsNull(t *testing.T) {
	db := newFixture(t, 6, fixtureRow{id: "a", userID: "u1", timestamp: "2026-11-02T07:00:00Z", weight: 71.5})
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	code, out, errOut := runBodyFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if raw["command"] != "body" || raw["status"] != "ok" {
		t.Fatalf("report = %+v", raw)
	}
	measurement, ok := raw["measurement"].(map[string]any)
	if !ok {
		t.Fatalf("measurement = %#v", raw["measurement"])
	}
	for _, field := range []string{"timestamp", "weight_kg", "user_id"} {
		if _, ok := measurement[field]; !ok {
			t.Errorf("measurement is missing %q", field)
		}
	}
	for _, field := range []string{"bmi", "body_fat_pct", "muscle_mass_kg", "visceral_fat_score"} {
		value, ok := measurement[field]
		if !ok {
			t.Errorf("measurement is missing %q", field)
			continue
		}
		if value != nil {
			t.Errorf("%s = %#v, want null", field, value)
		}
	}
}
