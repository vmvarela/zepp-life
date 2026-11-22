package main

import (
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	programName  = "zepp-life"
	appName      = "zepp-life-mcp"
	configFile   = "config.json"
	databaseFile = "zepp_life.db"

	// Base URLs of the Zepp/Huami cloud APIs. Tests override them with
	// httptest servers; no test ever reaches the real endpoints.
	huamiAPIBase = "https://api-mifit.huami.com"
	zeppAPIBase  = "https://api-mifit.zepp.com"

	// requestTimeout bounds every HTTP request and every SQLite operation.
	requestTimeout = 30 * time.Second
)

// environment carries everything machine-specific the commands read or write.
// Tests build their own so no real configuration, database, keyring, terminal
// or network endpoint is ever touched.
type environment struct {
	configPath   string
	databasePath string // used when there is no configuration file
	readSecret   func(service, account string) (string, error)
	writeSecret  func(service, account, value string) error
	promptSecret func(message string) (string, error) // hidden terminal input
	now          func() time.Time                     // injected clock
	httpClient   *http.Client
	huamiBase    string // auth probe and discovery endpoints
	zeppBase     string // body measurement endpoints
}

func defaultEnvironment() environment {
	configDir, dataDir := platformDirs()
	return environment{
		configPath:   filepath.Join(configDir, configFile),
		databasePath: filepath.Join(dataDir, databaseFile),
		readSecret:   platformReadSecret,
		writeSecret:  platformWriteSecret,
		promptSecret: platformPromptSecret,
		now:          time.Now,
		httpClient:   &http.Client{Timeout: requestTimeout},
		huamiBase:    huamiAPIBase,
		zeppBase:     zeppAPIBase,
	}
}

// platformDirs mirrors platformdirs >= 4 called with appname "zepp-life-mcp":
// the same two directories the Python tool uses for config.json and zepp_life.db.
func platformDirs() (configDir, dataDir string) {
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			base = filepath.Join(home, "AppData", "Local")
		}
		// platformdirs repeats the name because appauthor falls back to appname:
		// <LOCALAPPDATA>\zepp-life-mcp\zepp-life-mcp
		dir := filepath.Join(base, appName, appName)
		return dir, dir
	case "darwin":
		support := filepath.Join(home, "Library", "Application Support")
		return xdgDir("XDG_CONFIG_HOME", support), xdgDir("XDG_DATA_HOME", support)
	default:
		return xdgDir("XDG_CONFIG_HOME", filepath.Join(home, ".config")),
			xdgDir("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	}
}

// xdgDir returns $VAR/zepp-life-mcp when VAR is set, else fallback/zepp-life-mcp.
// platformdirs honours the XDG variables on Linux and on macOS alike.
func xdgDir(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return filepath.Join(v, appName)
	}
	return filepath.Join(fallback, appName)
}
