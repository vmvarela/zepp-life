package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestDoctorOnCleanInstallIsNormalAndExitsZero(t *testing.T) {
	env := newTestEnv(t)
	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}

	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
	}
	if report.Command != "doctor" || report.Status != "ok" {
		t.Fatalf("unexpected report: %+v", report)
	}
	if report.Config.Status != string(configMissing) {
		t.Errorf("config.status = %q, want %q", report.Config.Status, configMissing)
	}
	if report.Database.Status != "missing" {
		t.Errorf("database.status = %q, want %q", report.Database.Status, "missing")
	}
	if report.Keyring.Status != "missing" {
		t.Errorf("keyring.status = %q, want %q", report.Keyring.Status, "missing")
	}
	for _, check := range []string{report.Config.Detail, report.Database.Detail, report.Keyring.Detail} {
		if !strings.Contains(check, "normal on a clean install") {
			t.Errorf("detail %q does not explain that a clean install looks like this", check)
		}
	}
}

func TestDoctorReportsHealthyLocalState(t *testing.T) {
	db := newFixture(t, 6, fixtureRow{id: "m1", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70.5})
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)
	env.readSecret = writeSecrets(map[string]string{
		keyringTokenAccount:  "super-secret-token-value",
		keyringUserIDAccount: "u1",
	})

	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	if strings.Contains(stdout.String(), "super-secret-token-value") {
		t.Fatal("the token value leaked into the output")
	}

	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
	}
	if report.Status != "ok" {
		t.Fatalf("status = %q, want ok: %+v", report.Status, report)
	}
	if report.Config.Status != string(configPresent) || report.Config.Mode != modeCloudSession {
		t.Errorf("config = %+v", report.Config)
	}
	if report.Config.Timezone != "Europe/Madrid" {
		t.Errorf("timezone = %q", report.Config.Timezone)
	}
	if report.Database.Status != "ready" || report.Database.SchemaVersion != expectedSchemaVersion {
		t.Errorf("database = %+v", report.Database)
	}
	if report.Keyring.Status != "found" || !report.Keyring.HasToken || !report.Keyring.HasUserID {
		t.Errorf("keyring = %+v", report.Keyring)
	}
}

func TestDoctorMalformedConfigurationExitsOne(t *testing.T) {
	env := newTestEnv(t)
	writeConfigFile(t, env.configPath, "{not json")

	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}

	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout.String())
	}
	if report.Status != "error" || report.Config.Status != string(configInvalid) {
		t.Fatalf("report = %+v", report)
	}
	if report.Database.Status != "skipped" {
		t.Errorf("database.status = %q, want skipped", report.Database.Status)
	}
	if report.Config.Detail == "" {
		t.Error("config detail does not explain the problem")
	}
}

func TestDoctorNeverModifiesAnUnsupportedDatabase(t *testing.T) {
	for _, version := range []int{5, 7} {
		t.Run("version "+strconv.Itoa(version), func(t *testing.T) {
			db := newFixture(t, version, fixtureRow{id: "m1", userID: "u1", timestamp: "2026-11-01T07:00:00Z", weight: 70.5})
			env := newTestEnv(t)
			env.databasePath = db
			writeValidConfig(t, env.configPath, db)
			before := snapshot(t, db)

			var stdout, stderr bytes.Buffer
			code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
			if code != exitError {
				t.Fatalf("exit = %d, want %d", code, exitError)
			}

			var report doctorReport
			if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
				t.Fatalf("output is not JSON: %v", err)
			}
			if report.Database.Status != "unsupported_version" {
				t.Fatalf("database.status = %q, want unsupported_version", report.Database.Status)
			}
			if report.Database.SchemaVersion != version {
				t.Errorf("schema_version = %d, want %d", report.Database.SchemaVersion, version)
			}
			if !strings.Contains(report.Database.Detail, "not modified") {
				t.Errorf("detail %q does not say the database was left alone", report.Database.Detail)
			}
			if after := snapshot(t, db); after != before {
				t.Fatalf("doctor changed the database:\nbefore %q\nafter  %q", before, after)
			}
		})
	}
}

func TestDoctorMissingDatabaseIsNormal(t *testing.T) {
	env := newTestEnv(t)
	writeValidConfig(t, env.configPath, env.databasePath)

	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Database.Status != "missing" {
		t.Fatalf("database = %+v", report.Database)
	}
}

func TestDoctorWithoutBodyTableExitsOne(t *testing.T) {
	db := newFixtureWithoutBodyTable(t, 6)
	env := newTestEnv(t)
	env.databasePath = db
	writeValidConfig(t, env.configPath, db)

	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Database.Status != "unreadable" || !strings.Contains(report.Database.Detail, bodyTable) {
		t.Fatalf("database = %+v", report.Database)
	}
}

func TestDoctorKeyringFailureExitsOne(t *testing.T) {
	env := newTestEnv(t)
	env.readSecret = func(service, account string) (string, error) {
		return "", errors.New("keychain is locked")
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor", "--json"}, &stdout, &stderr, env)
	if code != exitError {
		t.Fatalf("exit = %d, want %d", code, exitError)
	}
	var report doctorReport
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Keyring.Status != "error" || report.Status != "error" {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Keyring.Detail, "keychain is locked") {
		t.Errorf("detail = %q", report.Keyring.Detail)
	}
}

func TestDoctorHumanOutputExplainsCleanInstall(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"doctor"}, &stdout, &stderr, newTestEnv(t))
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	out := stdout.String()
	if !strings.Contains(out, "Result: OK") {
		t.Errorf("output has no result line:\n%s", out)
	}
	if got := strings.Count(out, "normal on a clean install"); got < 3 {
		t.Errorf("expected the three checks to explain a clean install, found %d:\n%s", got, out)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q", stderr.String())
	}
}
