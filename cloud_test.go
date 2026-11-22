package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The expected strings below were produced with Python's
// datetime.fromtimestamp(value, UTC).isoformat(): the stored bytes must stay
// identical so both tools write the same rows.
func TestPythonUTCTimestampMatchesPython(t *testing.T) {
	tests := []struct {
		seconds float64
		want    string
	}{
		{1762066800, "2025-11-02T07:00:00+00:00"},
		{1762066800.5, "2025-11-02T07:00:00.500000+00:00"},
		{1762066800.123456, "2025-11-02T07:00:00.123456+00:00"},
		{1762066800.000001, "2025-11-02T07:00:00.000001+00:00"},
		{1762066801, "2025-11-02T07:00:01+00:00"},
	}
	for _, test := range tests {
		got := pythonUTCTimestamp(time.Unix(0, epochMicros(test.seconds)*1000).UTC())
		if got != test.want {
			t.Errorf("pythonUTCTimestamp(%v) = %q, want %q", test.seconds, got, test.want)
		}
	}
}

func TestParseWeightItemsMapsEverySummaryField(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(`{
		"id": "w1",
		"generatedTime": 1762066800,
		"summary": {
			"weight": 71.5, "bmi": 22.1, "fatRate": 18.5, "muscleRate": 30.2,
			"bodyWaterRate": 55.4, "boneMass": 3.1, "visceralFat": 8,
			"metabolism": 1650, "muscleAge": 33
		}
	}`)}
	records, err := parseWeightItems(items, "123", time.UTC, fixedClock(), "2020-01-01", "2026-11-20")
	if err != nil {
		t.Fatalf("parseWeightItems: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	r := records[0]
	if r.id != "cloud_weight_123_w1" {
		t.Errorf("id = %q", r.id)
	}
	if r.timestampText != "2025-11-02T07:00:00+00:00" {
		t.Errorf("timestampText = %q", r.timestampText)
	}
	if r.localDate != "2025-11-02" {
		t.Errorf("localDate = %q", r.localDate)
	}
	if r.weight != 71.5 {
		t.Errorf("weight = %v", r.weight)
	}
	assertFloat(t, "bmi", r.bmi, 22.1)
	assertFloat(t, "bodyFatPct", r.bodyFatPct, 18.5)
	assertFloat(t, "muscleMassKg", r.muscleMassKg, 30.2)
	assertFloat(t, "waterPct", r.waterPct, 55.4)
	assertFloat(t, "boneMassKg", r.boneMassKg, 3.1)
	assertInt(t, "visceralFat", r.visceralFat, 8)
	assertInt(t, "metabolism", r.metabolism, 1650)
	assertInt(t, "muscleAge", r.muscleAge, 33)
}

func TestParseWeightItemsKeepsAbsentSummaryFieldsNull(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(`{
		"id": "w1", "generatedTime": 1762066800,
		"summary": {"weight": 70, "bmi": null, "fatRate": 0}
	}`)}
	records, err := parseWeightItems(items, "123", time.UTC, fixedClock(), "2020-01-01", "2026-11-20")
	if err != nil {
		t.Fatalf("parseWeightItems: %v", err)
	}
	r := records[0]
	if r.bmi != nil {
		t.Errorf("bmi = %v, want null", *r.bmi)
	}
	// A recorded zero is a value, not an absence, for plain float columns.
	if r.bodyFatPct == nil || *r.bodyFatPct != 0 {
		t.Errorf("bodyFatPct = %v, want 0", r.bodyFatPct)
	}
	// Integer columns follow Python's truthiness: zero stores no value.
	if r.visceralFat != nil || r.metabolism != nil || r.muscleAge != nil {
		t.Errorf("integer fields should be null: %+v", r)
	}
}

func TestParseWeightItemsUsesTheInjectedClockWhenGeneratedTimeIsMissing(t *testing.T) {
	items := []json.RawMessage{json.RawMessage(`{"summary": {"weight": 70}}`)}
	records, err := parseWeightItems(items, "123", time.UTC, fixedClock(), "2020-01-01", "2026-11-20")
	if err != nil {
		t.Fatalf("parseWeightItems: %v", err)
	}
	want := pythonUTCTimestamp(fixedClock().UTC())
	if records[0].timestampText != want {
		t.Errorf("timestampText = %q, want %q", records[0].timestampText, want)
	}
	if records[0].id != "cloud_weight_123_1795176000" {
		t.Errorf("id = %q, want the epoch seconds of the injected clock", records[0].id)
	}
}

func TestParseWeightItemsRejectsInvalidWeights(t *testing.T) {
	tests := []struct {
		name    string
		summary string
		want    string
	}{
		{"missing", `{}`, "weight is missing"},
		{"null", `{"weight": null}`, "weight is missing"},
		{"zero", `{"weight": 0}`, "not greater than zero"},
		{"negative", `{"weight": -3}`, "not greater than zero"},
		{"not a number", `{"weight": "heavy"}`, "not a number"},
		{"empty string", `{"weight": ""}`, "not a number"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			items := []json.RawMessage{json.RawMessage(
				`{"id": "w1", "generatedTime": 1762066800, "summary": ` + test.summary + `}`,
			)}
			_, err := parseWeightItems(items, "123", time.UTC, fixedClock(), "2020-01-01", "2026-11-20")
			if err == nil {
				t.Fatal("error = nil, want a clear failure")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to mention %q", err, test.want)
			}
			if !strings.Contains(err.Error(), "cloud_weight_123_w1") {
				t.Errorf("error = %q, want it to name the record", err)
			}
		})
	}
}

func TestParseWeightItemsRejectsMalformedFields(t *testing.T) {
	tests := []struct {
		name string
		item string
		want string
	}{
		{"generatedTime is a string", `{"id": "w", "generatedTime": "yesterday", "summary": {"weight": 70}}`, "not usable"},
		{"generatedTime is text", `{"id": "w", "generatedTime": true, "summary": {"weight": 70}}`, "not usable"},
		{"bmi is text", `{"id": "w", "generatedTime": 1762066800, "summary": {"weight": 70, "bmi": "x"}}`, "bmi is not a number"},
		{"visceralFat is a fraction string", `{"id": "w", "generatedTime": 1762066800, "summary": {"weight": 70, "visceralFat": "8.5"}}`, "not an integer"},
		{"id is an object", `{"id": {"a": 1}, "generatedTime": 1762066800, "summary": {"weight": 70}}`, "id must be a string or a number"},
		{"item is not an object", `"just a string"`, "not usable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseWeightItems([]json.RawMessage{json.RawMessage(test.item)},
				"123", time.UTC, fixedClock(), "2020-01-01", "2026-11-20")
			if err == nil {
				t.Fatal("error = nil, want a clear failure")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Errorf("error = %q, want it to mention %q", err, test.want)
			}
		})
	}
}

func TestParseWeightItemsFiltersByLocalDateInclusively(t *testing.T) {
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}
	// 2025-11-01T06:30Z is 07:30 in Madrid on the day before the window.
	items := []json.RawMessage{
		json.RawMessage(`{"id": "early", "generatedTime": 1761978600, "summary": {"weight": 69}}`), // 2025-11-01 local
		json.RawMessage(`{"id": "edge", "generatedTime": 1762068600, "summary": {"weight": 70}}`),  // 2025-11-02 local, window start
		json.RawMessage(`{"id": "late", "generatedTime": 1762254000, "summary": {"weight": 71}}`),  // 2025-11-04 local, past the end
	}
	records, err := parseWeightItems(items, "123", madrid, fixedClock(), "2025-11-02", "2025-11-03")
	if err != nil {
		t.Fatalf("parseWeightItems: %v", err)
	}
	if len(records) != 1 || records[0].id != "cloud_weight_123_edge" {
		t.Fatalf("records = %+v, want only the in-range edge record", records)
	}
	// The same record is kept when the window covers its local date exactly.
	records, err = parseWeightItems(items, "123", madrid, fixedClock(), "2025-11-02", "2025-11-04")
	if err != nil {
		t.Fatalf("parseWeightItems: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want the two inside the widened window", len(records))
	}
	// Out-of-range records are filtered before their summary is read, so a
	// broken summary outside the window never fails the sync.
	broken := []json.RawMessage{
		json.RawMessage(`{"id": "old", "generatedTime": 1000000000, "summary": {}}`),
	}
	records, err = parseWeightItems(broken, "123", madrid, fixedClock(), "2025-11-02", "2025-11-03")
	if err != nil {
		t.Fatalf("out-of-range record must not fail: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %+v, want none", records)
	}
}

func TestParseWeightItemsLocalDateAcrossDSTBoundary(t *testing.T) {
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Fatal(err)
	}
	// Europe/Madrid springs forward on 2026-03-29 (02:00 -> 03:00).
	items := []json.RawMessage{
		// 2026-03-28T23:30Z is 2026-03-29T00:30 in Madrid (before the jump).
		json.RawMessage(`{"id": "a", "generatedTime": 1774740600, "summary": {"weight": 70}}`),
	}
	records, err := parseWeightItems(items, "123", madrid, fixedClock(), "2026-03-29", "2026-03-29")
	if err != nil {
		t.Fatalf("parseWeightItems: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want the DST-day record", len(records))
	}
	if records[0].localDate != "2026-03-29" {
		t.Errorf("localDate = %q, want 2026-03-29", records[0].localDate)
	}
	if records[0].timestampText != "2026-03-28T23:30:00+00:00" {
		t.Errorf("timestampText = %q", records[0].timestampText)
	}
}

func TestItemIDSuffixFollowsPythonStringification(t *testing.T) {
	tests := []struct {
		name string
		item string
		want string
	}{
		{"string id", `{"id": "abc", "generatedTime": 1}`, "abc"},
		{"number id", `{"id": 42, "generatedTime": 1}`, "42"},
		{"integer seconds", `{"generatedTime": 1762066800}`, "1762066800"},
		{"integral float", `{"generatedTime": 1762066800.0}`, "1762066800.0"},
		{"fraction", `{"generatedTime": 1762066800.5}`, "1762066800.5"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var item weightItem
			if err := json.Unmarshal([]byte(test.item), &item); err != nil {
				t.Fatal(err)
			}
			got, err := itemIDSuffix(item, time.Unix(1762066800, 0).UTC())
			if err != nil {
				t.Fatalf("itemIDSuffix: %v", err)
			}
			if got != test.want {
				t.Errorf("itemIDSuffix() = %q, want %q (Python prints it this way)", got, test.want)
			}
		})
	}
}

func TestProbeUsesTheExactEndpointAndHeaders(t *testing.T) {
	var (
		gotPath  string
		gotQuery string
		gotHead  http.Header
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery, gotHead = r.URL.Path, r.URL.RawQuery, r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	env := newTestEnv(t)
	env.huamiBase = server.URL
	if err := newCloudAPI(env, "synthetic-token").probe(); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if gotPath != "/v1/sport/run/history.json" || gotQuery != "limit=1" {
		t.Errorf("request = %s?%s", gotPath, gotQuery)
	}
	for header, want := range map[string]string{
		"apptoken":     "synthetic-token",
		"appPlatform":  "web",
		"appname":      "com.xiaomi.hm.health",
		"Content-Type": "application/json",
	} {
		if value := gotHead.Get(header); value != want {
			t.Errorf("%s = %q, want %q", header, value, want)
		}
	}
}

// TestCloudClientNeverFollowsRedirectsWithTheToken proves the apptoken header
// cannot leak to another host: the redirect policy is disabled on the cloud
// client itself, even when the injected test client still follows redirects.
func TestCloudClientNeverFollowsRedirectsWithTheToken(t *testing.T) {
	var (
		mu     sync.Mutex
		leaked []string // what the redirect target would have received
	)
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.Header.Get("apptoken"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/collect", http.StatusFound)
	}))
	defer origin.Close()

	env := newTestEnv(t)
	env.huamiBase = origin.URL
	// The injected client keeps the default redirect policy on purpose.
	env.httpClient = &http.Client{Timeout: 5 * time.Second}

	err := newCloudAPI(env, "synthetic-token").probe()
	if err == nil || !strings.Contains(err.Error(), "HTTP 302") {
		t.Fatalf("probe error = %v, want the 3xx reported instead of followed", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("the token reached the redirect target: %v", leaked)
	}
}

func TestProbeReportsRejectedAndUnreachableTokens(t *testing.T) {
	t.Run("401 suggests replace-token", func(t *testing.T) {
		stub := &cloudStub{probeCode: http.StatusUnauthorized}
		env := newTestEnv(t)
		defer stub.install(&env).Close()
		err := newCloudAPI(env, "t").probe()
		if err == nil || !strings.Contains(err.Error(), "--replace-token") {
			t.Fatalf("probe error = %v, want it to point at setup --replace-token", err)
		}
	})
	t.Run("500 is reported as the token check", func(t *testing.T) {
		stub := &cloudStub{probeCode: http.StatusInternalServerError}
		env := newTestEnv(t)
		defer stub.install(&env).Close()
		err := newCloudAPI(env, "t").probe()
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("probe error = %v", err)
		}
	})
	t.Run("network failure is actionable", func(t *testing.T) {
		env := newTestEnv(t)
		env.huamiBase = "http://127.0.0.1:1" // unroutable port
		err := newCloudAPI(env, "t").probe()
		if err == nil || !strings.Contains(err.Error(), "cannot reach the Zepp API") {
			t.Fatalf("probe error = %v", err)
		}
	})
}

func TestDiscoverUserIDUsesInjectedClockAndFirstDigitUID(t *testing.T) {
	stub := &cloudStub{bandData: []any{
		map[string]any{"uid": "not-digits"},
		map[string]any{"uid": 987654},
		map[string]any{"uid": "123456"},
	}}
	env := newTestEnv(t)
	server := stub.install(&env)
	defer server.Close()

	uid, err := newCloudAPI(env, "t").discoverUserID(fixedClock().In(time.UTC))
	if err != nil {
		t.Fatalf("discoverUserID: %v", err)
	}
	if uid != "987654" {
		t.Errorf("uid = %q, want the first all-digit uid", uid)
	}
	if len(stub.hits()) != 1 || !strings.Contains(stub.hits()[0], "/v1/data/band_data.json") {
		t.Errorf("hits = %v", stub.hits())
	}
	if !strings.Contains(stub.hits()[0], "from_date=2026-10-21") ||
		!strings.Contains(stub.hits()[0], "to_date=2026-11-20") {
		t.Errorf("date window missing from %q", stub.hits()[0])
	}
}

func TestDiscoverUserIDFindsNothingIsAnError(t *testing.T) {
	for name, bandData := range map[string]any{
		"no uid":       []any{map[string]any{"uid": "abc"}},
		"empty list":   []any{},
		"not a list":   "base64-not-a-list",
		"missing data": nil,
	} {
		t.Run(name, func(t *testing.T) {
			stub := &cloudStub{bandData: bandData}
			env := newTestEnv(t)
			defer stub.install(&env).Close()
			if _, err := newCloudAPI(env, "t").discoverUserID(fixedClock()); err == nil {
				t.Fatal("error = nil, want an actionable failure")
			}
		})
	}
}

func TestFetchWeightItemsUsesTheExactEndpointAndLimit(t *testing.T) {
	stub := &cloudStub{items: []any{map[string]any{"id": "w", "generatedTime": 1, "summary": map[string]any{"weight": 70}}}}
	env := newTestEnv(t)
	server := stub.install(&env)
	defer server.Close()

	items, err := newCloudAPI(env, "t").fetchWeightItems("123456")
	if err != nil {
		t.Fatalf("fetchWeightItems: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	if len(stub.hits()) != 1 {
		t.Fatalf("hits = %v, want exactly one request", stub.hits())
	}
	hit := stub.hits()[0]
	if !strings.Contains(hit, "/users/123456/members/-1/weightRecords") {
		t.Errorf("path missing from %q", hit)
	}
	if !strings.Contains(hit, "limit=200") {
		t.Errorf("limit=200 missing from %q (the endpoint is not paginated)", hit)
	}
}

func TestFetchWeightItemsEdgeResponses(t *testing.T) {
	t.Run("no items key is an empty list", func(t *testing.T) {
		stub := &cloudStub{itemsAbsent: true}
		env := newTestEnv(t)
		defer stub.install(&env).Close()
		items, err := newCloudAPI(env, "t").fetchWeightItems("1")
		if err != nil || items != nil {
			t.Fatalf("items, err = %v, %v; want nil, nil", items, err)
		}
	})
	t.Run("items of the wrong type fail clearly", func(t *testing.T) {
		stub := &cloudStub{items: "not-a-list"}
		env := newTestEnv(t)
		defer stub.install(&env).Close()
		if _, err := newCloudAPI(env, "t").fetchWeightItems("1"); err == nil {
			t.Fatal("error = nil, want a clear failure")
		}
	})
	t.Run("HTTP 500 fails clearly", func(t *testing.T) {
		stub := &cloudStub{weightCode: http.StatusInternalServerError}
		env := newTestEnv(t)
		defer stub.install(&env).Close()
		_, err := newCloudAPI(env, "t").fetchWeightItems("1")
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
			t.Fatalf("error = %v", err)
		}
	})
}

func assertFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

func assertInt(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}
