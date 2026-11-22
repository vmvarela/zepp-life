package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
	_ "time/tzdata" // timezone names must resolve even without a system tz database
)

// Modes accepted by the Python configuration.
const (
	modeNotConfigured = "not_configured"
	modeExportFile    = "export_file"
	modeCloudSession  = "cloud_session"
)

type configState string

const (
	configPresent configState = "present"
	configMissing configState = "missing"
	configInvalid configState = "invalid"
)

// appConfig is the part of the Python configuration this build reads.
type appConfig struct {
	mode         string
	timezone     string
	databasePath string
	exportPath   string // absolute folder configured with --source export-file
}

// location resolves the configured IANA timezone. loadConfig has already
// validated it, so the fallback can only trigger if the tz database vanished
// underneath us.
func (c appConfig) location() *time.Location {
	loc, err := time.LoadLocation(c.timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// loadConfig reads config.json without creating, repairing or rewriting it.
// A missing file is not an error: a clean install has no configuration yet.
func loadConfig(path, fallbackDatabase string) (appConfig, configState, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return appConfig{mode: modeNotConfigured, timezone: "UTC", databasePath: fallbackDatabase}, configMissing, nil
	}
	if err != nil {
		return appConfig{}, configInvalid, fmt.Errorf("cannot read %s: %w", path, err)
	}

	var raw struct {
		Mode         *string `json:"mode"`
		Timezone     *string `json:"timezone"`
		DatabasePath *string `json:"database_path"`
		ExportPath   *string `json:"export_path"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return appConfig{}, configInvalid, fmt.Errorf("%s is not a usable JSON configuration: %w", path, err)
	}

	cfg := appConfig{mode: modeNotConfigured, timezone: "UTC", databasePath: fallbackDatabase}
	if raw.Mode != nil {
		cfg.mode = *raw.Mode
	}
	switch cfg.mode {
	case modeExportFile, modeCloudSession, modeNotConfigured:
	default:
		return appConfig{}, configInvalid, fmt.Errorf("unsupported mode %q in %s", cfg.mode, path)
	}

	if raw.Timezone != nil {
		cfg.timezone = *raw.Timezone
	}
	if cfg.timezone == "" {
		return appConfig{}, configInvalid, fmt.Errorf("empty timezone in %s", path)
	}
	if _, err := time.LoadLocation(cfg.timezone); err != nil {
		return appConfig{}, configInvalid, fmt.Errorf("unknown timezone %q in %s", cfg.timezone, path)
	}

	if raw.DatabasePath != nil {
		db := *raw.DatabasePath
		if db == "" {
			return appConfig{}, configInvalid, fmt.Errorf("empty database_path in %s", path)
		}
		// The Python tool always writes an absolute path; a relative one would
		// resolve against whichever directory the process happens to start in.
		if !filepath.IsAbs(db) {
			return appConfig{}, configInvalid, fmt.Errorf("database_path in %s must be absolute, got %q", path, db)
		}
		cfg.databasePath = db
	}

	// export_path is the folder configured for export_file mode. An empty or
	// absent value simply means no folder is configured yet; a non-empty one
	// must be absolute for the same reason as database_path.
	if raw.ExportPath != nil {
		exportDir := *raw.ExportPath
		if exportDir != "" {
			if !filepath.IsAbs(exportDir) {
				return appConfig{}, configInvalid, fmt.Errorf("export_path in %s must be absolute, got %q", path, exportDir)
			}
			cfg.exportPath = exportDir
		}
	}
	return cfg, configPresent, nil
}

// rewriteConfig applies mutate to the JSON object at path and writes the
// result back, preserving every field mutate does not touch. A missing file
// is created with the defaults the Python tool would apply on first load,
// inside a private directory when none exists yet. mutate reports whether
// anything changed; when it returns false the file is left untouched.
func rewriteConfig(path, fallbackDatabase string, mutate func(fields map[string]json.RawMessage, created bool) (bool, error)) error {
	data, readErr := os.ReadFile(path)
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		return fmt.Errorf("cannot read %s: %w", path, readErr)
	}
	created := readErr != nil

	fields := map[string]json.RawMessage{}
	if !created {
		trimmed := bytes.TrimSpace(data)
		// Only a JSON object can be updated field by field. A root of null
		// unmarshals into a nil map (which would panic on assignment) and an
		// array or scalar is not a configuration at all: report instead of
		// replacing the file or crashing.
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return fmt.Errorf("%s must contain a JSON object", path)
		}
		if err := json.Unmarshal(trimmed, &fields); err != nil {
			return fmt.Errorf("%s is not a usable JSON configuration: %w", path, err)
		}
	}

	changed, err := mutate(fields, created)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if created {
		fields["timezone"] = json.RawMessage(`"UTC"`)
		databasePath, err := json.Marshal(fallbackDatabase)
		if err != nil {
			return fmt.Errorf("cannot encode the default database path: %w", err)
		}
		fields["database_path"] = databasePath
	}

	// A clean install has no configuration directory yet. MkdirAll never
	// changes the mode of a directory that already exists.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("cannot create the configuration directory: %w", err)
	}
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode the configuration: %w", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}

// updateConfigForCloud makes the configuration describe a cloud session.
// Every other field of an existing file is preserved byte for byte in
// meaning: only the mode is rewritten, and a file that already says
// cloud_session is left untouched.
func updateConfigForCloud(path, fallbackDatabase string) error {
	return rewriteConfig(path, fallbackDatabase, func(fields map[string]json.RawMessage, created bool) (bool, error) {
		if !created {
			if mode, ok := fields["mode"]; ok && string(mode) == `"`+modeCloudSession+`"` {
				return false, nil // already a cloud session: nothing to rewrite
			}
		}
		fields["mode"] = json.RawMessage(`"` + modeCloudSession + `"`)
		return true, nil
	})
}

// updateConfigForExport makes the configuration describe an export folder.
// It switches mode to export_file and stores exportDir as the absolute
// export_path, preserving every other field. A file that already points at
// the same folder in export_file mode is left untouched. This never writes a
// credential: the export source needs none.
func updateConfigForExport(path, fallbackDatabase, exportDir string) error {
	encodedDir, err := json.Marshal(exportDir)
	if err != nil {
		return fmt.Errorf("cannot encode the export folder path: %w", err)
	}
	return rewriteConfig(path, fallbackDatabase, func(fields map[string]json.RawMessage, created bool) (bool, error) {
		if !created {
			if mode, ok := fields["mode"]; ok &&
				string(mode) == `"`+modeExportFile+`"` &&
				string(fields["export_path"]) == string(encodedDir) {
				return false, nil
			}
		}
		fields["mode"] = json.RawMessage(`"` + modeExportFile + `"`)
		fields["export_path"] = encodedDir
		return true, nil
	})
}
