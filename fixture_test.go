package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// bodyMeasurementsDDL is the table zepp-life-mcp creates at schema version 6.
// Fixtures are synthetic: no real health data is ever used.
const bodyMeasurementsDDL = `CREATE TABLE body_measurements (
	id TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	source_type TEXT NOT NULL,
	source_record_id TEXT,
	user_id TEXT NOT NULL,
	device_id TEXT,
	device_key TEXT NOT NULL DEFAULT '',
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
	local_date TEXT,
	UNIQUE(user_id, timestamp, device_key)
)`

type fixtureRow struct {
	id        string
	userID    string
	timestamp string
	weight    float64
	device    string
	bodyFat   *float64
}

// newFixture writes a synthetic database at the requested schema version and
// returns its path. The file lives in the test's own temporary directory.
func newFixture(t *testing.T, version int, rows ...fixtureRow) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), databaseFile)
	db, err := sql.Open("sqlite", fixtureDSN(path))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(bodyMeasurementsDDL); err != nil {
		t.Fatalf("create fixture table: %v", err)
	}
	for _, row := range rows {
		var bodyFat, deviceID any
		if row.bodyFat != nil {
			bodyFat = *row.bodyFat
		}
		if row.device != "" {
			deviceID = row.device
		}
		_, err := db.Exec(`
INSERT INTO body_measurements (
	id, provider, source_type, user_id, device_id, device_key, timezone,
	timestamp, weight_kg, body_fat_pct, local_date
) VALUES (?, 'zepp_life', 'cloud_session', ?, ?, ?, 'UTC', ?, ?, ?, ?)`,
			row.id, row.userID, deviceID, row.device, row.timestamp, row.weight, bodyFat,
			row.timestamp[:10],
		)
		if err != nil {
			t.Fatalf("insert fixture row %s: %v", row.id, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		t.Fatalf("set fixture version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return path
}

// newFixtureWithoutBodyTable writes a database at the given version that has
// no body measurement table at all.
func newFixtureWithoutBodyTable(t *testing.T, version int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), databaseFile)
	db, err := sql.Open("sqlite", fixtureDSN(path))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE other_table (id TEXT)"); err != nil {
		t.Fatalf("create fixture table: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		t.Fatalf("set fixture version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close fixture: %v", err)
	}
	return path
}

// fixtureDSN is the read-write URI used only to build fixtures; the program
// itself only ever opens databases with readOnlyDSN.
func fixtureDSN(path string) string {
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return "file:" + escapeURIPath(p)
}

// writeSecrets returns a keyring reader backed by a map. Tests never touch the
// real keyring of this machine.
func writeSecrets(secrets map[string]string) func(service, account string) (string, error) {
	return func(service, account string) (string, error) {
		if service != keyringService {
			return "", fmt.Errorf("unexpected keyring service %q", service)
		}
		if value, ok := secrets[account]; ok {
			return value, nil
		}
		return "", errSecretNotFound
	}
}

// storeSecrets returns a keyring writer backed by the same map, so a test can
// read back whatever the program decided to store.
func storeSecrets(secrets map[string]string) func(service, account, value string) error {
	return func(service, account, value string) error {
		if service != keyringService {
			return fmt.Errorf("unexpected keyring service %q", service)
		}
		if value == "" {
			return fmt.Errorf("empty value for %q", account)
		}
		secrets[account] = value
		return nil
	}
}

// fixedClock is the injected "now" every test uses: 2026-11-20T12:00:00Z.
func fixedClock() time.Time {
	return time.Date(2026, time.November, 20, 12, 0, 0, 0, time.UTC)
}

// newTestEnv builds an environment pointing at empty temporary locations with
// synthetic keyring, clock and network defaults. The placeholder base URLs are
// unroutable: a test that forgets to override them fails instead of reaching
// the real cloud.
func newTestEnv(t *testing.T) environment {
	t.Helper()
	secrets := map[string]string{}
	return environment{
		configPath:   filepath.Join(t.TempDir(), configFile),
		databasePath: filepath.Join(t.TempDir(), databaseFile),
		readSecret:   writeSecrets(secrets),
		writeSecret:  storeSecrets(secrets),
		promptSecret: func(string) (string, error) {
			return "", errors.New("the prompt must not be used in this test")
		},
		now:        fixedClock,
		httpClient: &http.Client{Timeout: 5 * time.Second},
		huamiBase:  "http://huami.invalid",
		zeppBase:   "http://zepp.invalid",
	}
}

// newTestEnvWithSecrets builds a test environment whose keyring already holds
// the given values (a shared map that reads and writes go through).
func newTestEnvWithSecrets(t *testing.T, secrets map[string]string) environment {
	t.Helper()
	env := newTestEnv(t)
	env.readSecret = writeSecrets(secrets)
	env.writeSecret = storeSecrets(secrets)
	return env
}

func writeConfigFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// jsonString returns s encoded as a JSON string literal, so a temporary path
// with backslashes on Windows can be spliced into a hand-written
// configuration without producing invalid JSON.
func jsonString(t *testing.T, s string) string {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("encode %q as JSON: %v", s, err)
	}
	return string(raw)
}

// writeValidConfig stores the configuration the Python tool would write for a
// cloud session pointing at the given database.
func writeValidConfig(t *testing.T, path, databasePath string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"mode":          modeCloudSession,
		"timezone":      "Europe/Madrid",
		"database_path": databasePath,
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// snapshot fingerprints a database file and its directory so a read can be
// proven not to have changed any byte or created any sidecar file.
func snapshot(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database for snapshot: %v", err)
	}
	sum := sha256.Sum256(data)
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("list database directory: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return hex.EncodeToString(sum[:]) + "|" + strings.Join(names, ",")
}

// cloudStub is a synthetic Zepp cloud behind httptest. Tests point both API
// bases at it, record every request and answer with fixed payloads, so no test
// ever reaches the real endpoints.
type cloudStub struct {
	mu          sync.Mutex
	probeCode   int // 0 means 200
	weightCode  int // 0 means 200
	bandData    any // payload of {"data": ...}
	items       any // payload of {"items": ...}
	itemsAbsent bool
	requests    []string
}

func (s *cloudStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
	s.mu.Unlock()

	switch {
	case r.URL.Path == "/v1/sport/run/history.json":
		code := s.probeCode
		if code == 0 {
			code = http.StatusOK
		}
		w.WriteHeader(code)
	case r.URL.Path == "/v1/data/band_data.json":
		writeJSON(w, map[string]any{"data": s.bandData})
	case strings.HasSuffix(r.URL.Path, "/weightRecords"):
		if code := s.weightCode; code != 0 && code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		if s.itemsAbsent {
			writeJSON(w, map[string]any{})
			return
		}
		writeJSON(w, map[string]any{"items": s.items})
	default:
		http.NotFound(w, r)
	}
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// install starts the stub and points the environment at it.
func (s *cloudStub) install(env *environment) *httptest.Server {
	server := httptest.NewServer(s)
	env.huamiBase = server.URL
	env.zeppBase = server.URL
	return server
}

// hits returns the requests received so far.
func (s *cloudStub) hits() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...)
}

// countPaths counts requests whose path contains the given fragment.
func (s *cloudStub) countPaths(fragment string) int {
	n := 0
	for _, hit := range s.hits() {
		if strings.Contains(hit, fragment) {
			n++
		}
	}
	return n
}
