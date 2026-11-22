package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// fetchActivityData performs the single bounded daily-summary request for the
// inclusive [start, end] window and returns the raw `data` payload. One
// request, bounded by maxResponseBytes and the shared 30s timeout, without
// redirects — the same client rules as every other cloud call.
func (c *cloudAPI) fetchActivityData(userID, start, end string) (json.RawMessage, error) {
	query := url.Values{
		"query_type":  {"summary"},
		"device_type": {"android_phone"},
		"userid":      {userID},
		"from_date":   {start},
		"to_date":     {end},
	}
	resp, err := c.get(c.env.huamiBase, "/v1/data/band_data.json", query)
	if err != nil {
		return nil, err
	}
	defer drainAndClose(resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("Zepp rejected the stored token (HTTP %d); run `zepp-life setup --replace-token` to enter a new one",
			resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the daily activity request failed with HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&payload); err != nil {
		return nil, fmt.Errorf("the daily activity response is not usable: %w", err)
	}
	return payload.Data, nil
}

// activityEntry is one normalized outer entry: a canonical date plus the raw
// summary as received (a JSON object or base64 text).
type activityEntry struct {
	date    string
	summary json.RawMessage
}

// parseActivityData normalizes the outer `data` payload into daily records.
//
// Preconditions (P): start and end are canonical, ordered dates; raw is the
// `data` field of one response bounded by maxResponseBytes.
//
// Postconditions (Q): every returned record has a canonical date inside the
// inclusive [start, end] window — entries outside it are dropped even when
// the service overreturns — and metrics inside the shared sanity bounds.
// Entries with no usable activity at all (no summary, a valid summary object
// without `stp`, an empty or falsy `stp`) are skipped; a malformed outer
// payload, a present-but-malformed summary, or an explicit null/malformed
// metric fails the whole parse, so nothing partial reaches the database and
// a date is never skipped past by the cursor while its summary was broken.
//
// Termination: the payload is capped at maxResponseBytes and every entry is
// visited a bounded number of times (once as an outer value, once as a
// summary), so all loops finish.
func parseActivityData(raw json.RawMessage, start, end string) ([]activityRecord, error) {
	entries, err := activityEntries(raw, 0)
	if err != nil {
		return nil, err
	}
	records := make([]activityRecord, 0, len(entries))
	for _, entry := range entries {
		// Filter first: an out-of-range entry is never validated, so service
		// overreturn outside the window cannot fail the run.
		if entry.date < start || entry.date > end {
			continue
		}
		record, usable, err := activityRecordFromSummary(entry)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.date, err)
		}
		if !usable {
			continue // no summary, no stp or a falsy stp: no usable activity
		}
		records = append(records, record)
	}
	return records, nil
}

// malformedActivityPayload marks an unusable outer payload — as opposed to a
// skippable per-entry problem — which fails the whole sync.
func malformedActivityPayload(detail string) error {
	return fmt.Errorf("the daily activity payload is malformed: %s", detail)
}

// activityEntries turns the outer payload into entries. The payload may be a
// base64 JSON array, a direct JSON array, or an object keyed by date; null
// or absent data is a normal empty result. Nested base64 is followed at most
// depth levels, so the recursion is bounded.
func activityEntries(raw json.RawMessage, depth int) ([]activityEntry, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, nil // no data: a normal empty result
	}
	if depth > 2 {
		return nil, malformedActivityPayload("the data field nests too deeply")
	}
	switch trimmed[0] {
	case '"':
		var encoded string
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return nil, malformedActivityPayload("the data field is not text")
		}
		decoded, err := decodeBase64Text(encoded)
		if err != nil {
			return nil, malformedActivityPayload("the data field is not valid base64")
		}
		return activityEntries(decoded, depth+1)
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, malformedActivityPayload("the data field is not an entry list")
		}
		entries := make([]activityEntry, 0, len(items))
		for i, item := range items {
			entry, err := activityEntryFrom(item, "")
			if err != nil {
				return nil, fmt.Errorf("entry %d: %w", i+1, err)
			}
			entries = append(entries, entry)
		}
		return entries, nil
	case '{':
		var byDate map[string]json.RawMessage
		if err := json.Unmarshal(raw, &byDate); err != nil {
			return nil, malformedActivityPayload("the data field is not an object")
		}
		keys := make([]string, 0, len(byDate))
		for key := range byDate {
			keys = append(keys, key)
		}
		// Sorted: map order is random, and both the records and any error
		// must come out the same way on every run.
		sort.Strings(keys)
		entries := make([]activityEntry, 0, len(keys))
		for _, key := range keys {
			date, err := normalizeDate(key)
			if err != nil {
				return nil, malformedActivityPayload(
					"the data field has a date key that is not a date")
			}
			entry, err := activityEntryFrom(byDate[key], date)
			if err != nil {
				return nil, fmt.Errorf("entry %s: %w", date, err)
			}
			entries = append(entries, entry)
		}
		return entries, nil
	}
	return nil, malformedActivityPayload("the data field is not a list, object or base64 text")
}

// activityEntryFrom reads one entry object. fallbackDate (already canonical,
// from a date-keyed object) is used when the entry carries no date of its
// own; a summary field may be absent — an inline `stp` object makes the
// entry its own summary — and without any date the entry is a malformed
// outer payload.
func activityEntryFrom(raw json.RawMessage, fallbackDate string) (activityEntry, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return activityEntry{}, errors.New("the entry is not an object")
	}
	date := fallbackDate
	dateRaw, hasDate := fields["date_time"]
	if !hasDate {
		dateRaw, hasDate = fields["date"]
	}
	if hasDate && strings.TrimSpace(string(dateRaw)) != "null" {
		var text string
		if err := json.Unmarshal(dateRaw, &text); err != nil {
			return activityEntry{}, errors.New("the entry date is not text")
		}
		parsed, err := normalizeDate(text)
		if err != nil {
			return activityEntry{}, errors.New("the entry date is not a date")
		}
		date = parsed
	} else if date == "" {
		return activityEntry{}, errors.New("the entry has no date")
	}
	summary, ok := fields["summary"]
	if !ok {
		if _, inline := fields["stp"]; inline {
			summary = raw // the entry is the summary itself
		}
	}
	return activityEntry{date: date, summary: summary}, nil
}

// activitySummaryObject reads the summary of one entry. skip reports that the
// entry simply carries no summary at all (absent, null or empty), which is
// valid "no activity" data and never an error. A summary that is present must
// decode to a JSON object: invalid non-empty base64, invalid decoded JSON or
// any other shape is an error, so a malformed in-range summary aborts the
// sync instead of being dropped while the cursor advances past its date.
// Neither the encoded nor the decoded content ever appears in an error.
func activitySummaryObject(raw json.RawMessage) (map[string]json.RawMessage, bool, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil, true, nil // no summary at all: nothing to import
	}
	content := []byte(trimmed)
	if trimmed[0] == '"' {
		var encoded string
		if err := json.Unmarshal(content, &encoded); err != nil {
			return nil, false, errors.New("the summary is not valid JSON")
		}
		if strings.TrimSpace(encoded) == "" {
			return nil, true, nil // empty summary text: nothing to import
		}
		decoded, err := decodeBase64Text(encoded)
		if err != nil {
			return nil, false, errors.New("the summary is not valid base64")
		}
		content = decoded
	}
	if strings.TrimSpace(string(content)) == "" {
		return nil, true, nil // decoded to nothing: nothing to import
	}
	if !json.Valid(content) {
		return nil, false, errors.New("the summary is not valid JSON")
	}
	var object map[string]json.RawMessage
	// A JSON null unmarshals into a nil map without error: that is the
	// unsupported shape it is, not an empty summary.
	if err := json.Unmarshal(content, &object); err != nil || object == nil {
		return nil, false, errors.New("the summary is not a JSON object")
	}
	return object, false, nil
}

// stpActivityState classifies the `stp` value of a decoded summary with
// Python-like truthiness: falsy values (null, false, 0, "", [], {}) mean the
// day holds no usable activity and the entry is skipped. A truthy value that
// is not a non-empty JSON object cannot be mapped to steps, distance or
// calories and is reported as an error instead of being dropped, so no day
// that might carry activity is skipped while the cursor advances past it.
func stpActivityState(raw json.RawMessage) (falsy bool, object map[string]json.RawMessage, malformed bool) {
	trimmed := strings.TrimSpace(string(raw))
	switch trimmed {
	case "", "null", "false", `""`, "[]":
		return true, nil, false
	}
	var number float64
	if err := json.Unmarshal([]byte(trimmed), &number); err == nil {
		if number == 0 {
			return true, nil, false // 0 and 0.0 carry no activity
		}
		return false, nil, true // truthy scalar (5, 0.5, ...): unusable shape
	}
	var array []json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &array); err == nil {
		if len(array) == 0 {
			return true, nil, false
		}
		return false, nil, true // truthy array: unusable shape
	}
	var stp map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &stp); err != nil || stp == nil {
		return false, nil, true // not a JSON object at all
	}
	if len(stp) == 0 {
		return true, nil, false // {} with any spacing carries no activity
	}
	return false, stp, false
}

// activityRecordFromSummary maps one in-range entry to a record. The entry is
// skipped (usable=false) only when it carries no usable activity at all: no
// summary, a valid summary object without `stp`, or an empty/falsy `stp`. A
// summary that is present but malformed — undecodable base64, invalid JSON,
// an unsupported shape, a truthy non-object `stp` — is an error, as is a
// null/malformed/out-of-range metric: nothing that might carry activity is
// dropped while the cursor could advance past its date. No value ever
// appears in the error.
func activityRecordFromSummary(entry activityEntry) (activityRecord, bool, error) {
	summary, skip, err := activitySummaryObject(entry.summary)
	if err != nil {
		return activityRecord{}, false, err
	}
	if skip {
		return activityRecord{}, false, nil
	}
	stpRaw, hasSTP := summary["stp"]
	if !hasSTP {
		return activityRecord{}, false, nil // valid summary without stp: no activity
	}
	falsy, stp, malformed := stpActivityState(stpRaw)
	if malformed {
		return activityRecord{}, false, errors.New("the stp field is not a JSON object")
	}
	if falsy {
		return activityRecord{}, false, nil
	}
	steps, err := activityMetric(stp, "ttl", maxActivitySteps)
	if err != nil {
		return activityRecord{}, false, err
	}
	distance, err := activityMetric(stp, "dis", maxActivityDistM)
	if err != nil {
		return activityRecord{}, false, err
	}
	kcal, err := activityMetric(stp, "cal", maxActivityKcal)
	if err != nil {
		return activityRecord{}, false, err
	}
	return activityRecord{
		date:       entry.date,
		steps:      int64(steps), // truncated towards zero, like the CSV parser
		distanceM:  distance,     // stp.dis is metres: no unit heuristic here
		activeKcal: kcal,
	}, true, nil
}

// activityMetric reads one day-summary metric. An absent key means "not
// recorded" and defaults to 0; an explicit null or any value that is not a
// finite, non-negative number within max is an error. The raw value never
// appears in the message: it would be health data.
func activityMetric(stp map[string]json.RawMessage, key string, max float64) (float64, error) {
	raw, ok := stp[key]
	if !ok {
		return 0, nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return 0, fmt.Errorf("%s is null", key)
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err != nil {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return 0, fmt.Errorf("%s is not a number", key)
		}
		parsed, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if err != nil {
			return 0, fmt.Errorf("%s is not a number", key)
		}
		number = parsed
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > max {
		return 0, fmt.Errorf("%s is out of range", key)
	}
	return number, nil
}

// decodeBase64Text accepts the standard alphabet with or without padding and
// the URL-safe variants, which is what API payloads have been seen to use.
func decodeBase64Text(text string) ([]byte, error) {
	text = strings.TrimSpace(text)
	for _, encoding := range []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	} {
		if decoded, err := encoding.DecodeString(text); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("not base64")
}
