package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runSetupFor(t *testing.T, env environment, extraArgs ...string) (int, string, string) {
	t.Helper()
	args := append([]string{"setup"}, extraArgs...)
	var stdout, stderr strings.Builder
	code := run(args, &stdout, &stderr, env)
	return code, stdout.String(), stderr.String()
}

// mustNotPrompt fails the test if anything tries to ask for the token.
func mustNotPrompt(t *testing.T) func(string) (string, error) {
	t.Helper()
	return func(string) (string, error) {
		t.Fatal("the prompt must not be used")
		return "", nil
	}
}

func configExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

func TestSetupReusesCompleteSessionWithoutPromptOrNetwork(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "ok" || report.Session != "reused" || report.UserID != "123456" {
		t.Fatalf("report = %+v", report)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("a complete session must not touch the network: %v", hits)
	}
	if !configExists(t, env.configPath) {
		t.Fatal("the configuration was not created")
	}
	if strings.Contains(out, "synthetic-token") {
		t.Fatalf("the token leaked into the output:\n%s", out)
	}
	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if err != nil || state != configPresent || cfg.mode != modeCloudSession {
		t.Fatalf("config = %+v, %v, %v", cfg, state, err)
	}
}

func TestSetupInteractionRequiredForJSONNeverPrompts(t *testing.T) {
	env := newTestEnv(t)
	env.promptSecret = mustNotPrompt(t)
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "interaction_required" {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.Detail, "terminal") {
		t.Errorf("detail = %q, want it to say what to do", report.Detail)
	}
	if configExists(t, env.configPath) {
		t.Fatal("nothing must be written when a token is required")
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSetupInteractionRequiredWithoutATerminal(t *testing.T) {
	env := newTestEnv(t)
	env.promptSecret = func(string) (string, error) { return "", errNoTTY }
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env)
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	if !strings.Contains(out, "interaction_required") || !strings.Contains(out, "terminal") {
		t.Fatalf("human output = %q", out)
	}
	if configExists(t, env.configPath) {
		t.Fatal("nothing must be written without a token")
	}
}

func TestSetupPromptsValidatesAndStoresTheSession(t *testing.T) {
	secrets := map[string]string{}
	env := newTestEnvWithSecrets(t, secrets)
	prompted := 0
	env.promptSecret = func(message string) (string, error) {
		prompted++
		if !strings.Contains(message, "Zepp app token") || !strings.Contains(message, "user.huami.com") {
			t.Errorf("prompt message = %q, want it to name the token and where to get it", message)
		}
		return "  synthetic-token-value  ", nil
	}
	stub := &cloudStub{bandData: []any{map[string]any{"uid": "123456"}}}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if prompted != 1 {
		t.Fatalf("prompted %d times, want exactly one hidden prompt", prompted)
	}
	// The secret never appears in any output, and it is stored trimmed.
	if strings.Contains(out, "synthetic-token-value") || strings.Contains(errOut, "synthetic-token-value") {
		t.Fatalf("the token leaked:\nstdout: %s\nstderr: %s", out, errOut)
	}
	if secrets[keyringTokenAccount] != "synthetic-token-value" {
		t.Errorf("stored token = %q, want the trimmed prompt value", secrets[keyringTokenAccount])
	}
	if secrets[keyringUserIDAccount] != "123456" {
		t.Errorf("stored user id = %q", secrets[keyringUserIDAccount])
	}
	if !strings.Contains(out, "configured") || !strings.Contains(out, "sync --type body_measurements") {
		t.Fatalf("human output = %q", out)
	}
	if hits := stub.hits(); len(hits) != 2 ||
		!strings.Contains(hits[0], "/v1/sport/run/history.json") ||
		!strings.Contains(hits[1], "/v1/data/band_data.json") {
		t.Fatalf("hits = %v, want probe then discovery", hits)
	}

	// The session is complete now: a second run reuses it without prompting.
	env.promptSecret = mustNotPrompt(t)
	code, out, errOut = runSetupFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("second setup exit = %d (stderr: %s)", code, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.Session != "reused" || report.UserID != "123456" {
		t.Fatalf("second report = %+v", report)
	}
	if strings.Contains(out, "synthetic-token-value") {
		t.Fatalf("the token leaked into JSON:\n%s", out)
	}
}

func TestSetupDoesNotStoreARejectedToken(t *testing.T) {
	secrets := map[string]string{}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = func(string) (string, error) { return "rejected-token", nil }
	stub := &cloudStub{probeCode: http.StatusUnauthorized}
	server := stub.install(&env)
	defer server.Close()

	// The token is rejected before anything is written: neither the token
	// nor the explicit user id may reach the keyring.
	code, out, errOut := runSetupFor(t, env, "--user-id", "999999")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	if !strings.Contains(out, "error") || !strings.Contains(out, "--replace-token") {
		t.Fatalf("human output = %q", out)
	}
	if strings.Contains(out, "rejected-token") || strings.Contains(errOut, "rejected-token") {
		t.Fatalf("the rejected token leaked:\n%s\n%s", out, errOut)
	}
	if len(secrets) != 0 {
		t.Fatalf("a rejected token or its user id was stored: %v", secrets)
	}
	if configExists(t, env.configPath) {
		t.Fatal("the configuration must not be written for a rejected token")
	}
}

func TestSetupNeverSwitchesAnExportFileConfiguration(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	// Platform-neutral absolute folder: loadConfig rejects a relative
	// export_path, and "/data/export" is not absolute on Windows.
	original := `{"mode": "export_file", "export_path": ` +
		jsonString(t, filepath.Join(t.TempDir(), "export")) + `, "timezone": "UTC"}`
	writeConfigFile(t, env.configPath, original)
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, modeExportFile) {
		t.Fatalf("report = %+v", report)
	}
	if got := snapshotConfig(t, env.configPath); got != original {
		t.Fatalf("config changed to %q", got)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSetupPreservesEveryOtherConfigurationField(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	db := env.databasePath
	writeConfigFile(t, env.configPath, `{
  "mode": "not_configured",
  "region": "us",
  "timezone": "Europe/Madrid",
  "database_path": `+jsonString(t, db)+`,
  "export_path": null,
  "stale_after_minutes": 90
}`)

	code, _, errOut := runSetupFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	var fields map[string]json.RawMessage
	data, err := os.ReadFile(env.configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("config is not JSON: %v", err)
	}
	if string(fields["mode"]) != `"cloud_session"` {
		t.Errorf("mode = %s", fields["mode"])
	}
	for field, want := range map[string]string{
		"region":              `"us"`,
		"timezone":            `"Europe/Madrid"`,
		"export_path":         `null`,
		"stale_after_minutes": `90`,
	} {
		if got := string(fields[field]); got != want {
			t.Errorf("%s = %s, want %s (preserved)", field, got, want)
		}
	}
	if string(fields["database_path"]) != jsonString(t, db) {
		t.Errorf("database_path = %s, want the original path", fields["database_path"])
	}
}

func TestSetupReplaceTokenPromptsAndReplacesTheStoredToken(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "old-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	prompted := 0
	env.promptSecret = func(string) (string, error) {
		prompted++
		return "new-token", nil
	}
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env, "--replace-token")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if prompted != 1 {
		t.Fatalf("prompted %d times, want one", prompted)
	}
	if secrets[keyringTokenAccount] != "new-token" {
		t.Errorf("stored token = %q, want the replacement", secrets[keyringTokenAccount])
	}
	if !strings.Contains(out, "configured") || !strings.Contains(out, "123456") {
		t.Fatalf("human output = %q", out)
	}
	// The known user id means only the probe runs.
	if hits := stub.hits(); len(hits) != 1 || !strings.Contains(hits[0], "/v1/sport/run/history.json") {
		t.Fatalf("hits = %v, want only the probe", hits)
	}
	if strings.Contains(out, "new-token") || strings.Contains(errOut, "new-token") {
		t.Fatalf("the replacement token leaked:\n%s\n%s", out, errOut)
	}
}

// TestSetupStoresAnExplicitUserIDWithANewToken covers the flow where a fresh
// token is entered together with --user-id: the id must reach the keyring
// exactly once, after the probe accepted the token, and discovery must never
// run because the id is already known.
func TestSetupStoresAnExplicitUserIDWithANewToken(t *testing.T) {
	secrets := map[string]string{}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = func(string) (string, error) { return "synthetic-token", nil }
	// Discovery would answer with a different id: it must never be asked.
	stub := &cloudStub{bandData: []any{map[string]any{"uid": "999999"}}}
	server := stub.install(&env)
	defer server.Close()

	// Count writes of the user id to prove it happens exactly once.
	userIDWrites := 0
	store := env.writeSecret
	env.writeSecret = func(service, account, value string) error {
		if account == keyringUserIDAccount {
			userIDWrites++
		}
		return store(service, account, value)
	}

	code, out, errOut := runSetupFor(t, env, "--user-id", "424242")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if secrets[keyringTokenAccount] != "synthetic-token" {
		t.Errorf("stored token = %q, want the prompted value", secrets[keyringTokenAccount])
	}
	if secrets[keyringUserIDAccount] != "424242" {
		t.Errorf("explicit user id was not persisted: %v", secrets)
	}
	if userIDWrites != 1 {
		t.Errorf("user id written %d times, want exactly 1", userIDWrites)
	}
	// Only the probe runs: an explicit id skips discovery entirely.
	if hits := stub.hits(); len(hits) != 1 || !strings.Contains(hits[0], "/v1/sport/run/history.json") {
		t.Fatalf("hits = %v, want only the probe", hits)
	}
	if !strings.Contains(out, "424242") || !strings.Contains(out, "configured") {
		t.Errorf("human output = %q", out)
	}
	if strings.Contains(out, "synthetic-token") || strings.Contains(errOut, "synthetic-token") {
		t.Errorf("token leaked:\nstdout: %s\nstderr: %s", out, errOut)
	}
	if !configExists(t, env.configPath) {
		t.Error("the configuration was not written")
	}
}

func TestSetupStoresAProvidedUserIDWithoutAnyRequest(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	userIDWrites := 0
	store := env.writeSecret
	env.writeSecret = func(service, account, value string) error {
		if account == keyringUserIDAccount {
			userIDWrites++
		}
		return store(service, account, value)
	}

	code, out, errOut := runSetupFor(t, env, "--user-id", "424242", "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if secrets[keyringUserIDAccount] != "424242" {
		t.Errorf("stored user id = %q", secrets[keyringUserIDAccount])
	}
	if secrets[keyringTokenAccount] != "synthetic-token" {
		t.Errorf("stored token = %q, want it untouched", secrets[keyringTokenAccount])
	}
	if userIDWrites != 1 {
		t.Errorf("user id written %d times, want exactly 1", userIDWrites)
	}
	// An explicit id is stored as it is: no discovery, no other request.
	if got := stub.countPaths("/v1/data/band_data.json"); got != 0 {
		t.Errorf("discovery ran %d times, want 0", got)
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.UserID != "424242" || report.Status != "ok" {
		t.Fatalf("report = %+v", report)
	}
}

func TestSetupDiscoversTheUserIDForASavedToken(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	stub := &cloudStub{bandData: []any{map[string]any{"uid": "555"}}}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	if secrets[keyringUserIDAccount] != "555" {
		t.Errorf("stored user id = %q, want the discovered one", secrets[keyringUserIDAccount])
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if report.UserID != "555" || report.Status != "ok" {
		t.Fatalf("report = %+v", report)
	}
}

func TestSetupDiscoveryFailureIsActionableAndKeepsTheToken(t *testing.T) {
	secrets := map[string]string{keyringTokenAccount: "synthetic-token"}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	stub := &cloudStub{bandData: []any{}}
	server := stub.install(&env)
	defer server.Close()

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "--user-id") {
		t.Fatalf("report = %+v", report)
	}
	if secrets[keyringTokenAccount] != "synthetic-token" {
		t.Errorf("the validated token must stay stored: %v", secrets)
	}
	if configExists(t, env.configPath) {
		t.Fatal("the configuration must wait for a complete session")
	}
}

func TestSetupRejectsAMalformedConfiguration(t *testing.T) {
	env := newTestEnv(t)
	original := `{"mode": `
	writeConfigFile(t, env.configPath, original)

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" {
		t.Fatalf("report = %+v", report)
	}
	if got := snapshotConfig(t, env.configPath); got != original {
		t.Fatalf("config changed to %q", got)
	}
}

func TestSetupRejectsUnexpectedArguments(t *testing.T) {
	env := newTestEnv(t)
	var stdout, stderr strings.Builder
	code := run([]string{"setup", "extra"}, &stdout, &stderr, env)
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "unexpected argument") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestSetupRejectsANonNumericUserID(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	stub := &cloudStub{}
	server := stub.install(&env)
	defer server.Close()

	for _, bad := range []string{"abc", "12 34", "12;34", "../etc", "12a"} {
		var stdout, stderr strings.Builder
		code := run([]string{"setup", "--user-id", bad, "--json"}, &stdout, &stderr, env)
		if code != exitUsage {
			t.Errorf("--user-id %q exit = %d, want %d", bad, code, exitUsage)
		}
		if !strings.Contains(stderr.String(), "--user-id") {
			t.Errorf("--user-id %q stderr = %q", bad, stderr.String())
		}
	}
	// Nothing was stored, written or sent anywhere.
	if secrets[keyringUserIDAccount] != "123456" {
		t.Errorf("stored user id changed: %q", secrets[keyringUserIDAccount])
	}
	if configExists(t, env.configPath) {
		t.Error("the configuration must not be written for an invalid flag")
	}
	if hits := stub.hits(); len(hits) != 0 {
		t.Errorf("requests sent: %v", hits)
	}
}

func TestSetupCreatesAMissingConfigurationDirectory(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	env.configPath = filepath.Join(t.TempDir(), "absent", "dir", configFile)

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, errOut)
	}
	cfg, state, err := loadConfig(env.configPath, env.databasePath)
	if err != nil || state != configPresent || cfg.mode != modeCloudSession {
		t.Fatalf("config = %+v, %v, %v", cfg, state, err)
	}
	if !strings.Contains(out, "reused") {
		t.Fatalf("report = %q", out)
	}
}

func TestSetupFailsCleanlyOnANullConfiguration(t *testing.T) {
	secrets := map[string]string{
		keyringTokenAccount:  "synthetic-token",
		keyringUserIDAccount: "123456",
	}
	env := newTestEnvWithSecrets(t, secrets)
	env.promptSecret = mustNotPrompt(t)
	writeConfigFile(t, env.configPath, "null")

	code, out, errOut := runSetupFor(t, env, "--json")
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitError, errOut)
	}
	var report setupReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if report.Status != "error" || !strings.Contains(report.Detail, "JSON object") {
		t.Fatalf("report = %+v", report)
	}
	if got := snapshotConfig(t, env.configPath); got != "null" {
		t.Fatalf("file changed to %q", got)
	}
}

func TestUpdateConfigForCloudLeavesACloudSessionFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFile)
	// Deliberately unusual formatting: a rewrite would normalise it.
	original := `{"mode":"cloud_session","region":"us"}`
	writeConfigFile(t, path, original)

	if err := updateConfigForCloud(path, "/tmp/db.sqlite"); err != nil {
		t.Fatalf("updateConfigForCloud: %v", err)
	}
	if got := snapshotConfig(t, path); got != original {
		t.Fatalf("file rewritten to %q, want byte-identical", got)
	}
}

func TestUpdateConfigForCloudCreatesAMissingFileWithDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), configFile)
	// Platform-neutral absolute path: loadConfig rejects a relative one, and
	// "/data/zepp_life.db" has no drive letter on Windows.
	fallback := filepath.Join(t.TempDir(), databaseFile)

	if err := updateConfigForCloud(path, fallback); err != nil {
		t.Fatalf("updateConfigForCloud: %v", err)
	}
	cfg, state, err := loadConfig(path, filepath.Join(t.TempDir(), "elsewhere.db"))
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig = %+v, %v, %v", cfg, state, err)
	}
	if cfg.mode != modeCloudSession || cfg.timezone != "UTC" || cfg.databasePath != fallback {
		t.Fatalf("config = %+v, want the cloud defaults", cfg)
	}
}

func TestUpdateConfigForCloudCreatesTheParentDirectory(t *testing.T) {
	// A clean install has no configuration directory yet.
	path := filepath.Join(t.TempDir(), "missing", "nested", configFile)
	// Platform-neutral absolute path: loadConfig rejects a relative one, and
	// "/data/zepp_life.db" has no drive letter on Windows.
	dbPath := filepath.Join(t.TempDir(), databaseFile)
	if err := updateConfigForCloud(path, dbPath); err != nil {
		t.Fatalf("updateConfigForCloud: %v", err)
	}
	cfg, state, err := loadConfig(path, filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil || state != configPresent {
		t.Fatalf("loadConfig = %+v, %v, %v", cfg, state, err)
	}
	if cfg.mode != modeCloudSession || cfg.databasePath != dbPath {
		t.Fatalf("config = %+v", cfg)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Dir(path))
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("directory mode = %o, want 700 (private)", perm)
		}
	}
}

func TestUpdateConfigForCloudRejectsNonObjectRoots(t *testing.T) {
	// A root that is not a JSON object must be reported, never panic and
	// never be silently replaced.
	tests := map[string]string{
		"null":   `null`,
		"array":  `[]`,
		"scalar": `42`,
		"empty":  ``,
		"broken": `{"mode":`,
	}
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), configFile)
			writeConfigFile(t, path, contents)

			err := updateConfigForCloud(path, "/data/zepp_life.db")
			if err == nil {
				t.Fatal("error = nil, want a clear refusal")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want it to name the file", err)
			}
			if got := snapshotConfig(t, path); got != contents {
				t.Fatalf("file changed to %q", got)
			}
		})
	}
}
