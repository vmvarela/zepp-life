package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// dateLayout is the only date format this build accepts and writes.
	dateLayout = "2006-01-02"
	// weightLimit bounds the single weight request. The endpoint offers no
	// pagination, so at most this many records are fetched per sync.
	weightLimit = "200"
	// defaultStartDate is the window start when no cursor is stored yet.
	defaultStartDate = "2020-01-01"
	// maxResponseBytes bounds every response body this build reads.
	maxResponseBytes = 8 << 20

	providerZeppLife   = "zepp_life"
	sourceCloudSession = "cloud_session"
)

// cloudAPI talks to the Zepp cloud with the exact endpoints, headers and
// timeout of the Python adapter. Tests point both base URLs at httptest
// servers, so no test ever reaches the real endpoints.
type cloudAPI struct {
	env    environment
	token  string
	client *http.Client
}

func newCloudAPI(env environment, token string) *cloudAPI {
	client := env.httpClient
	if client == nil {
		client = &http.Client{Timeout: requestTimeout}
	}
	// Copy the injected client (tests supply their own transport) but never
	// follow redirects: the apptoken header must not travel to another host.
	// ErrUseLastResponse hands the 3xx response back to the caller, which
	// reports it as a failed status like any other.
	noRedirects := *client
	noRedirects.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &cloudAPI{env: env, token: token, client: &noRedirects}
}

// get issues one GET with the Zepp headers. The token travels only in the
// apptoken header: it never appears in the URL, so no error can echo it.
func (c *cloudAPI) get(base, path string, query url.Values) (*http.Response, error) {
	endpoint := base + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("cannot build the Zepp request: %w", err)
	}
	req.Header.Set("apptoken", c.token)
	req.Header.Set("appPlatform", "web")
	req.Header.Set("appname", "com.xiaomi.hm.health")
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the Zepp API: %w", err)
	}
	return resp, nil
}

// drainAndClose releases a response without reading an unbounded body.
func drainAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

// probe checks that the token is accepted. Any HTTP 200 counts as success.
func (c *cloudAPI) probe() error {
	resp, err := c.get(c.env.huamiBase, "/v1/sport/run/history.json", url.Values{"limit": {"1"}})
	if err != nil {
		return err
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("Zepp rejected the stored token (HTTP %d); run `zepp-life setup --replace-token` to enter a new one",
			resp.StatusCode)
	}
	return fmt.Errorf("the token check failed with HTTP %d", resp.StatusCode)
}

// discoverUserID finds the account id from the last 30 days of band summary
// data, taking the first all-digit uid the way the Python adapter does.
func (c *cloudAPI) discoverUserID(today time.Time) (string, error) {
	query := url.Values{
		"query_type":  {"summary"},
		"device_type": {"android_phone"},
		"from_date":   {today.AddDate(0, 0, -30).Format(dateLayout)},
		"to_date":     {today.Format(dateLayout)},
	}
	resp, err := c.get(c.env.huamiBase, "/v1/data/band_data.json", query)
	if err != nil {
		return "", err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("the user id lookup failed with HTTP %d", resp.StatusCode)
	}

	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return "", fmt.Errorf("the user id lookup returned an unreadable response: %w", err)
	}
	var entries []json.RawMessage
	if len(payload.Data) == 0 || string(payload.Data) == "null" ||
		json.Unmarshal(payload.Data, &entries) != nil {
		// The Python adapter treats a non-list payload the same way: no uid.
		return "", errors.New("no user id found in the Zepp account data")
	}
	for _, entry := range entries {
		var item struct {
			UID json.RawMessage `json:"uid"`
		}
		if json.Unmarshal(entry, &item) != nil {
			continue // not an object: the Python adapter skips it
		}
		if uid, ok := allDigitsID(item.UID); ok {
			return uid, nil
		}
	}
	return "", errors.New("no user id found in the Zepp account data")
}

// allDigitsID accepts a uid only when it is a digit-only string or a digit-only
// JSON number, which is what str(value).isdigit() accepts in Python.
func allDigitsID(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return "", false
	}
	if strings.HasPrefix(trimmed, `"`) {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return "", false
		}
		trimmed = s
	}
	if !allDigits(trimmed) {
		return "", false
	}
	return trimmed, true
}

// allDigits reports whether s is a non-empty run of ASCII digits. Zepp user
// ids are numbers: discovery only ever returns them and setup validates
// --user-id against this same rule, so arbitrary input can never be stored
// or reach a URL path.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// fetchWeightItems performs the single weight request. The response holds at
// most weightLimit items and is never paginated.
func (c *cloudAPI) fetchWeightItems(userID string) ([]json.RawMessage, error) {
	path := "/users/" + url.PathEscape(userID) + "/members/-1/weightRecords"
	resp, err := c.get(c.env.zeppBase, path, url.Values{"limit": {weightLimit}})
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the body measurement request failed with HTTP %d", resp.StatusCode)
	}

	var payload map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("the body measurement response is not usable: %w", err)
	}
	raw, ok := payload["items"]
	if !ok || string(raw) == "null" {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, errors.New("the body measurement response does not contain an item list")
	}
	return items, nil
}

// weightItem is the wire shape of one weight record. Numbers are kept as raw
// text so ids can be built exactly the way Python stringifies them.
type weightItem struct {
	ID            json.RawMessage            `json:"id"`
	GeneratedTime *json.Number               `json:"generatedTime"`
	Summary       map[string]json.RawMessage `json:"summary"`
}

// weightRecord is one measurement ready for the database.
type weightRecord struct {
	id            string
	userID        string
	timestampText string // Python-compatible UTC ISO-8601
	localDate     string // local date in the configured timezone
	weight        float64
	bmi           *float64
	bodyFatPct    *float64
	muscleMassKg  *float64
	waterPct      *float64
	boneMassKg    *float64
	visceralFat   *int64
	metabolism    *int64
	muscleAge     *int64
}

// parseWeightItems turns the bounded item list into records.
//
// Records outside the inclusive [start, end] local-date window are filtered
// out before their summary is read, so they are never processed and never
// counted. Every in-range record must be complete: an unusable weight fails
// the whole sync instead of silently losing the measurement.
func parseWeightItems(items []json.RawMessage, userID string, loc *time.Location, now time.Time, start, end string) ([]weightRecord, error) {
	// Termination: items is one bounded response of at most weightLimit
	// entries; the loop runs once over that fixed list.
	records := make([]weightRecord, 0, len(items))
	for i, raw := range items {
		record, inRange, err := parseWeightItem(raw, i, userID, loc, now, start, end)
		if err != nil {
			return nil, err
		}
		if !inRange {
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func parseWeightItem(raw json.RawMessage, index int, userID string, loc *time.Location, now time.Time, start, end string) (weightRecord, bool, error) {
	var item weightItem
	if err := json.Unmarshal(raw, &item); err != nil {
		return weightRecord{}, false, fmt.Errorf("record %d: the response item is not usable: %w", index+1, err)
	}

	// Timestamp: generatedTime is Unix seconds (fraction allowed); a missing
	// value falls back to the injected clock.
	nanos := now.UnixNano()
	switch {
	case item.GeneratedTime != nil:
		seconds, err := item.GeneratedTime.Float64()
		if err != nil {
			return weightRecord{}, false, fmt.Errorf("record %d: generatedTime is not a number", index+1)
		}
		micros := epochMicros(seconds)
		nanos = (micros/1e6)*1e9 + (micros%1e6)*1e3
	}
	stamp := time.Unix(0, nanos).UTC()

	suffix, err := itemIDSuffix(item, stamp)
	if err != nil {
		return weightRecord{}, false, fmt.Errorf("record %d: %v", index+1, err)
	}
	record := weightRecord{
		id:            fmt.Sprintf("cloud_weight_%s_%s", userID, suffix),
		userID:        userID,
		timestampText: pythonUTCTimestamp(stamp),
		localDate:     stamp.In(loc).Format(dateLayout),
	}
	// Inclusive filter on the local date, compared as YYYY-MM-DD text.
	if record.localDate < start || record.localDate > end {
		return weightRecord{}, false, nil
	}

	summary := item.Summary
	if record.weight, err = requiredWeight(summary); err != nil {
		return weightRecord{}, false, fmt.Errorf("record %d (%s): %v", index+1, record.id, err)
	}
	optionalFloats := []struct {
		key    string
		target **float64
	}{
		{"bmi", &record.bmi},
		{"fatRate", &record.bodyFatPct},
		{"muscleRate", &record.muscleMassKg},
		{"bodyWaterRate", &record.waterPct},
		{"boneMass", &record.boneMassKg},
	}
	for _, field := range optionalFloats {
		value, err := optionalFloat(summary, field.key)
		if err != nil {
			return weightRecord{}, false, fmt.Errorf("record %d (%s): %v", index+1, record.id, err)
		}
		*field.target = value
	}
	optionalInts := []struct {
		key    string
		target **int64
	}{
		{"visceralFat", &record.visceralFat},
		{"metabolism", &record.metabolism},
		{"muscleAge", &record.muscleAge},
	}
	for _, field := range optionalInts {
		value, err := optionalInt(summary, field.key)
		if err != nil {
			return weightRecord{}, false, fmt.Errorf("record %d (%s): %v", index+1, record.id, err)
		}
		*field.target = value
	}
	return record, true, nil
}

// itemIDSuffix is "<id or generatedTime>": the item id when present (a string
// or a number, formatted like Python's f-string), otherwise the raw
// generatedTime, otherwise the epoch seconds of the resolved timestamp.
func itemIDSuffix(item weightItem, stamp time.Time) (string, error) {
	if len(item.ID) > 0 && string(item.ID) != "null" {
		var s string
		if json.Unmarshal(item.ID, &s) == nil {
			return s, nil
		}
		var number json.Number
		if json.Unmarshal(item.ID, &number) == nil {
			return number.String(), nil
		}
		return "", errors.New("id must be a string or a number")
	}
	if item.GeneratedTime != nil {
		text := item.GeneratedTime.String()
		if !strings.ContainsAny(text, ".eE") {
			return text, nil // integer seconds: Python prints them unchanged
		}
		value, err := item.GeneratedTime.Float64()
		if err != nil {
			return "", errors.New("generatedTime is not a number")
		}
		formatted := strconv.FormatFloat(value, 'f', -1, 64)
		if !strings.Contains(formatted, ".") {
			// Python's str() of an integral float always keeps the ".0".
			formatted += ".0"
		}
		return formatted, nil
	}
	return strconv.FormatInt(stamp.Unix(), 10), nil
}

// epochMicros converts Unix seconds to whole microseconds, rounding to the
// nearest microsecond the way Python's fromtimestamp does. Seconds stay well
// inside float64's exact-integer range at microsecond resolution, so the
// stored text matches Python byte for byte.
func epochMicros(seconds float64) int64 {
	return int64(math.Round(seconds * 1e6))
}

// pythonUTCTimestamp renders an instant exactly like Python's
// datetime.isoformat() for a UTC datetime: "+00:00" offset, six fractional
// digits only when the microsecond part is non-zero, none otherwise.
func pythonUTCTimestamp(t time.Time) string {
	base := t.UTC().Format("2006-01-02T15:04:05")
	micros := t.UTC().Nanosecond() / 1000
	if micros == 0 {
		return base + "+00:00"
	}
	return fmt.Sprintf("%s.%06d+00:00", base, micros)
}

// requiredWeight reads summary.weight. A missing, null, non-numeric or
// non-positive weight is an error: the measurement must not be lost silently.
func requiredWeight(summary map[string]json.RawMessage) (float64, error) {
	raw, ok := summary["weight"]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return 0, errors.New("weight is missing from the summary")
	}
	weight, err := jsonFloat(raw)
	if err != nil {
		return 0, fmt.Errorf("weight is %v", err)
	}
	if weight <= 0 {
		return 0, fmt.Errorf("weight %g is not greater than zero", weight)
	}
	return weight, nil
}

// optionalFloat reads a nullable float column. Absent and null mean no value;
// anything else must be a number (numeric strings are accepted the way
// pydantic coerces them).
func optionalFloat(summary map[string]json.RawMessage, key string) (*float64, error) {
	raw, ok := summary[key]
	if !ok || strings.TrimSpace(string(raw)) == "null" {
		return nil, nil
	}
	value, err := jsonFloat(raw)
	if err != nil {
		return nil, fmt.Errorf("%s is %v", key, err)
	}
	return &value, nil
}

// optionalInt reads a nullable integer column with Python's truthiness rules:
// null, false, 0 and the empty string store no value; int(True) is 1; floats
// truncate towards zero; integer strings are accepted.
func optionalInt(summary map[string]json.RawMessage, key string) (*int64, error) {
	raw, ok := summary[key]
	if !ok {
		return nil, nil
	}
	switch strings.TrimSpace(string(raw)) {
	case "null", "false", `""`:
		return nil, nil
	case "true":
		one := int64(1)
		return &one, nil
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		if number == 0 {
			return nil, nil
		}
		value := int64(number)
		return &value, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if strings.TrimSpace(text) == "" {
			return nil, nil
		}
		value, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is not an integer", key)
		}
		return &value, nil
	}
	return nil, fmt.Errorf("%s is not an integer", key)
}

// jsonFloat reads a JSON number, also accepting numeric strings. null is
// rejected here: whether null means "no value" belongs to the caller.
func jsonFloat(raw json.RawMessage) (float64, error) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
			return value, nil
		}
	}
	return 0, fmt.Errorf("not a number (%s)", strings.TrimSpace(string(raw)))
}
