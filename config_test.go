package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadConfigMissingFileIsNormal(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFile)
	fallback := filepath.Join(t.TempDir(), databaseFile)

	cfg, state, err := loadConfig(path, fallback)
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if state != configMissing {
		t.Fatalf("state = %q, want %q", state, configMissing)
	}
	if cfg.mode != modeNotConfigured || cfg.timezone != "UTC" || cfg.databasePath != fallback {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if _, statErr := os.Stat(path); statErr == nil {
		t.Fatal("loadConfig created a configuration file")
	}
}

func TestLoadConfigReadsPythonConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFile)
	db := filepath.Join(t.TempDir(), "elsewhere", databaseFile)
	writeConfigFile(t, path, `{
  "mode": "cloud_session",
  "region": "eu",
  "timezone": "Europe/Madrid",
  "database_path": `+jsonString(t, db)+`,
  "logs_path": "/tmp/ignored.log",
  "auto_sync_on_start": false
}`)

	cfg, state, err := loadConfig(path, filepath.Join(t.TempDir(), databaseFile))
	if err != nil {
		t.Fatalf("loadConfig() error = %v", err)
	}
	if state != configPresent {
		t.Fatalf("state = %q, want %q", state, configPresent)
	}
	if cfg.mode != modeCloudSession {
		t.Errorf("mode = %q, want %q", cfg.mode, modeCloudSession)
	}
	if cfg.timezone != "Europe/Madrid" {
		t.Errorf("timezone = %q, want Europe/Madrid", cfg.timezone)
	}
	if cfg.databasePath != db {
		t.Errorf("databasePath = %q, want %q", cfg.databasePath, db)
	}
}

func TestLoadConfigDefaultsWhenFieldsAreAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFile)
	fallback := filepath.Join(t.TempDir(), databaseFile)
	writeConfigFile(t, path, `{"mode": "export_file"}`)

	cfg, state, err := loadConfig(path, fallback)
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig() = %+v, %q, %v", cfg, state, err)
	}
	if cfg.timezone != "UTC" || cfg.databasePath != fallback {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigRejectsBrokenConfigurations(t *testing.T) {
	tests := []struct {
		name     string
		contents string
	}{
		{name: "not json", contents: `{"mode": `},
		{name: "empty file", contents: ``},
		{name: "json array", contents: `[]`},
		{name: "wrong type", contents: `{"mode": 12}`},
		{name: "unknown mode", contents: `{"mode": "telepathy"}`},
		{name: "unknown timezone", contents: `{"timezone": "Mars/Olympus_Mons"}`},
		{name: "empty timezone", contents: `{"timezone": ""}`},
		{name: "empty database path", contents: `{"database_path": ""}`},
		{name: "relative database path", contents: `{"database_path": "data/zepp_life.db"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), configFile)
			writeConfigFile(t, path, test.contents)

			_, state, err := loadConfig(path, filepath.Join(t.TempDir(), databaseFile))
			if state != configInvalid {
				t.Fatalf("state = %q, want %q", state, configInvalid)
			}
			if err == nil {
				t.Fatal("loadConfig() error = nil, want an explanation")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestPlatformDirsHonourXDGVariables(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("XDG variables do not apply on Windows")
	}
	configHome := t.TempDir()
	dataHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_DATA_HOME", dataHome)

	configDir, dataDir := platformDirs()
	if want := filepath.Join(configHome, appName); configDir != want {
		t.Errorf("configDir = %q, want %q", configDir, want)
	}
	if want := filepath.Join(dataHome, appName); dataDir != want {
		t.Errorf("dataDir = %q, want %q", dataDir, want)
	}
}
