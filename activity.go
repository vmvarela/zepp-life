package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// typeDailyActivity is the export data type this build imports.
const typeDailyActivity = "daily_activity"

// sourceExportFile is the source_type stored on rows imported from an export
// folder; the value matches the mode name the Python tool writes.
const sourceExportFile = "export_file"

// maxExportFileBytes bounds one export CSV. Activity exports are small; a
// larger file is reported, never partially imported.
//
// The remaining limits bound one whole run (a setup validation or a sync
// import) over an export folder. Rationale for the caps: a real Zepp export
// holds a handful of CSVs and a few thousand rows, so 256 files, 64 MiB of
// reading and 100k rows sit orders of magnitude above any legitimate folder
// while keeping the work of a hostile one bounded. The directory-entry cap
// counts every name scanned — CSV or not — so the listing itself stops at a
// fixed number of entries instead of walking a folder stuffed with
// unrelated files.
const (
	maxExportFileBytes  = 8 << 20  // per file, read from the opened handle
	maxExportFiles      = 256      // candidate CSVs retained per run
	maxExportTotalBytes = 64 << 20 // bytes read across all candidates per run
	maxExportRecords    = 100_000  // parsed rows retained per run
	maxExportDirEntries = 4096     // directory entries scanned per run, of any name
)

// errExportLimit marks a failure caused by one of the limits above. Commands
// abort the whole run when they see it; doctor reports the folder as
// over_limit instead of a generic problem.
var errExportLimit = errors.New("export import limit exceeded")

// limitError carries a clean user-facing message while still reporting as
// errExportLimit through errors.Is.
type limitError struct{ detail string }

func (e *limitError) Error() string { return e.detail }
func (e *limitError) Unwrap() error { return errExportLimit }

// exportLimit builds a limit violation with an actionable message: the path
// and the counts, never a cell value.
func exportLimit(format string, args ...any) error {
	return &limitError{detail: fmt.Sprintf(format, args...)}
}

// importBudget bounds one setup validation or one sync import over an export
// folder: at most maxExportFiles candidate files (enforced while the folder
// is listed, before any path is retained), at most maxExportTotalBytes read
// across all of them, at most maxExportRecords parsed rows retained, and at
// most maxExportDirEntries directory entries scanned while listing the
// export root and its known activity subfolders. The first violation is kept
// in err so every later read fails fast, and callers abort before touching
// the configuration or the database.
//
// Invariant: while err == nil all counters are within their limits; they
// only grow, so each check shrinks the room left until a violation stops
// the run.
type importBudget struct {
	bytes   int64
	records int
	entries int
	err     error
}

// fail records the first violation and returns it (later ones keep the
// original message, so the reported cause is the first bound that broke).
func (b *importBudget) fail(err error) error {
	if b == nil {
		return err
	}
	if b.err == nil {
		b.err = err
	}
	return b.err
}

// accountBytes charges n freshly read bytes to the run's aggregate budget.
func (b *importBudget) accountBytes(path string, n int) error {
	if b == nil {
		return nil
	}
	if b.err != nil {
		return b.err
	}
	b.bytes += int64(n)
	if b.bytes > maxExportTotalBytes {
		return b.fail(exportLimit("%s: read %d bytes in total, over the %d bytes limit for one import",
			path, b.bytes, maxExportTotalBytes))
	}
	return nil
}

// addRecord charges one successfully parsed row to the run's record budget.
// It is called before the row is appended, so the retained slice never
// exceeds maxExportRecords.
func (b *importBudget) addRecord(path string) error {
	if b == nil {
		return nil
	}
	if b.err != nil {
		return b.err
	}
	b.records++
	if b.records > maxExportRecords {
		return b.fail(exportLimit("%s: parsed %d rows, over the %d rows limit for one import",
			path, b.records, maxExportRecords))
	}
	return nil
}

// addDirEntry charges one scanned directory entry — of any name, CSV or not —
// to the run's listing budget, so a folder stuffed with unrelated files
// stops the scan at a fixed number of entries instead of being walked to
// its end.
func (b *importBudget) addDirEntry(dir string) error {
	if b == nil {
		return nil
	}
	if b.err != nil {
		return b.err
	}
	b.entries++
	if b.entries > maxExportDirEntries {
		return b.fail(exportLimit("%s: scanned %d directory entries in total (CSV or not), over the %d limit for one export folder",
			dir, b.entries, maxExportDirEntries))
	}
	return nil
}

// cappedReader charges every byte read to the run's aggregate budget and
// stops at the per-file cap. The cap is enforced on the handle, not on the
// path: a file that grows after the stat check (or reports a small size) is
// cut off instead of read without bound. remaining starts at
// maxExportFileBytes+1 so a file of exactly the cap still ends in a clean
// EOF, while one byte more trips the limit.
type cappedReader struct {
	source    io.Reader
	path      string
	remaining int64
	budget    *importBudget
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.budget != nil && c.budget.err != nil {
		return 0, c.budget.err // fail fast once a limit broke
	}
	if c.remaining <= 0 {
		return 0, c.budget.fail(exportLimit("%s: reading stopped at the %d bytes per-file limit",
			c.path, maxExportFileBytes))
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.source.Read(p)
	if n > 0 {
		if accountErr := c.budget.accountBytes(c.path, n); accountErr != nil {
			return n, accountErr
		}
		c.remaining -= int64(n)
	}
	return n, err
}

// Sanity bounds: far above any plausible day, and low enough that every
// conversion below (steps to int64, kilometres to metres) stays exact.
const (
	maxActivitySteps = 1e12 // steps per day
	maxActivityDistM = 1e9  // metres per day
	maxActivityKcal  = 1e7  // kilocalories per day
)

// activitySubDirs are the one-level folders searched for activity CSVs. The
// search never recurses into subfolders of these.
var activitySubDirs = []string{"ACTIVITY", "activity", "steps", "STEPS"}

// Column aliases, matched case-insensitively against the header in the order
// given: the first alias present wins, exactly like the reference parser.
var (
	dateColumnAliases     = []string{"date", "day", "timestamp", "time"}
	stepsColumnAliases    = []string{"steps", "step", "step_count", "steps_count"}
	distanceColumnAliases = []string{"distance", "distance_m", "distance_meters", "dist"}
	kcalColumnAliases     = []string{"calories", "cal", "kcal", "active_calories", "energy"}
)

// dateLayouts are the date formats accepted in export CSVs, in the order the
// reference parser tries them: day/month is tested before month/day, so an
// ambiguous value like 05/11/2026 is read as 5 November.
var dateLayouts = []string{
	"2006-01-02",
	"2006/01/02",
	"02.01.2006",
	"02/01/2006", // DD/MM/YYYY
	"01/02/2006", // MM/DD/YYYY, only when DD/MM does not match
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02T15:04:05Z",
}

// activityRecord is one normalized daily activity row, before it is stored.
type activityRecord struct {
	date       string // canonical YYYY-MM-DD
	steps      int64
	distanceM  float64
	activeKcal float64
}

// activityColumns maps a recognized header to the column index of each field
// it understands; -1 means the column is absent.
type activityColumns struct {
	date, steps, distance, kcal int
	distanceInMeters            bool
}

func noActivityColumns() activityColumns {
	return activityColumns{date: -1, steps: -1, distance: -1, kcal: -1}
}

// discoverActivityCSVs returns every CSV this build considers for an activity
// import: files directly inside the known activity folders, plus every CSV in
// the export root (the root catch-all covers the *activity*.csv and
// *steps*.csv patterns). Subfolders are never entered.
//
// The listing streams one directory batch at a time: every entry it reads,
// CSV or not, is charged to a listing budget of maxExportDirEntries, so a
// folder stuffed with unrelated names stops the scan at a fixed count, and
// at most maxExportFiles distinct candidates are retained. Each candidate is
// lstat'ed (a symlink is never followed), non-regular candidates are
// rejected with an actionable error, and duplicates — the same file reached
// as ACTIVITY/x.csv and activity/x.csv on a case-insensitive filesystem —
// are dropped by file identity before the count matters. The fixed activity
// subfolders are rejected if they are a symlink or not a folder at all
// (only the explicitly selected export root may be a symlink). The result is
// sorted, so repeated runs read the files in the same order.
func discoverActivityCSVs(dir string) ([]string, error) {
	kept := make([]string, 0, maxExportFiles)
	var infos []fs.FileInfo
	add := func(path string) error {
		// Lstat, not stat: a symlink must be reported as a symlink instead
		// of being followed to whatever it points at.
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("cannot inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is a %s; export candidates must be regular CSV files",
				path, fileKind(info.Mode()))
		}
		for i, other := range infos {
			if os.SameFile(other, info) {
				// Duplicate spelling of a file already kept (ACTIVITY/ vs
				// activity/ on a case-insensitive filesystem). Keep the
				// lexicographically first spelling so the result does not
				// depend on the order the folders were listed in.
				if path < kept[i] {
					kept[i] = path
				}
				return nil
			}
		}
		if len(kept) >= maxExportFiles {
			return exportLimit("%s contains more than %d CSV candidate files; at most %d can be imported at once",
				dir, maxExportFiles, maxExportFiles)
		}
		kept = append(kept, path)
		infos = append(infos, info)
		return nil
	}
	// One listing budget spans the root and every activity subfolder, so the
	// entry limit covers the whole scan, not each folder on its own.
	budget := &importBudget{}
	if err := streamCandidateCSVs(dir, false, budget, add); err != nil {
		return nil, err
	}
	// Spellings in lexicographic order: on a case-insensitive filesystem
	// ACTIVITY/ and activity/ are one folder, only the first spelling seen is
	// scanned, and processing the smallest one first keeps the result
	// deterministic — the same preference the file dedup applies.
	subs := append([]string(nil), activitySubDirs...)
	sort.Strings(subs)
	var seenDirs []fs.FileInfo
	for _, sub := range subs {
		subPath := filepath.Join(dir, sub)
		info, err := os.Lstat(subPath)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // the folder is simply absent
			}
			return nil, fmt.Errorf("cannot inspect %s: %w", subPath, err)
		}
		// The fixed activity folders are chosen by this tool, not by the
		// user: a symlink or a non-folder under one of those names would
		// make the import follow an arbitrary tree, so it is rejected
		// instead of traversed. Only the explicitly selected export root
		// may be a symlink.
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return nil, fmt.Errorf("%s is a symlink; the fixed activity folders must be real directories, not links", subPath)
		case info.IsDir():
			// A real folder: scanned below.
		case info.Mode().IsRegular():
			return nil, fmt.Errorf("%s is a file; the fixed activity folders must be real directories", subPath)
		default:
			return nil, fmt.Errorf("%s is a %s; the fixed activity folders must be real directories",
				subPath, fileKind(info.Mode()))
		}
		duplicate := false
		for _, other := range seenDirs {
			if os.SameFile(other, info) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue // one folder reached through two spellings: entries are counted once
		}
		seenDirs = append(seenDirs, info)
		if err := streamCandidateCSVs(subPath, true, budget, add); err != nil {
			return nil, err
		}
	}
	sort.Strings(kept)
	return kept, nil
}

// streamCandidateCSVs reads one folder's entries in fixed-size batches and
// calls add for every regular-name candidate (a non-directory entry ending
// in .csv). Every entry read is charged to budget first, so the scan stops
// at maxExportDirEntries whatever the names look like; at most one batch of
// directory entries is held in memory and add decides when to stop. An
// optional folder (an activity subfolder that may simply not exist) is
// skipped when absent.
func streamCandidateCSVs(dir string, optional bool, budget *importBudget, add func(path string) error) error {
	handle, err := os.Open(dir)
	if err != nil {
		if optional && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if optional {
			return fmt.Errorf("cannot read %s: %w", dir, err)
		}
		return fmt.Errorf("cannot read the export folder %s: %w", dir, err)
	}
	defer handle.Close()
	for {
		entries, err := handle.ReadDir(64)
		for _, entry := range entries {
			// Charge before anything else: the bound is on entries scanned,
			// not on candidates kept.
			if entryErr := budget.addDirEntry(dir); entryErr != nil {
				return entryErr
			}
			if entry.IsDir() || !isCSVName(entry.Name()) {
				continue
			}
			if addErr := add(filepath.Join(dir, entry.Name())); addErr != nil {
				return addErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("cannot read %s: %w", dir, err)
		}
	}
}

// fileKind names the file type of a non-regular candidate for the error
// message, so the user knows what to remove or replace.
func fileKind(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode&fs.ModeNamedPipe != 0:
		return "FIFO"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeDevice != 0:
		return "device"
	case mode&fs.ModeIrregular != 0:
		return "irregular file"
	case mode&fs.ModeDir != 0:
		return "directory"
	}
	return "non-regular file"
}

func isCSVName(name string) bool {
	return strings.EqualFold(filepath.Ext(name), ".csv")
}

// openActivityCSV opens one candidate file without following a symlink,
// verifies the opened handle is a regular file within the per-file cap,
// strips a UTF-8 BOM and reads its header. recognized is false when the
// header does not identify a date column and at least one activity metric
// column; such a file is ignored, not an error. Reads go through a capped
// reader charged to budget, so neither this header read nor any later one
// can run past the per-file or aggregate byte limits. The caller closes the
// returned file when it is non-nil.
func openActivityCSV(path string, budget *importBudget) (*os.File, *csv.Reader, []string, activityColumns, bool, error) {
	if budget == nil {
		budget = &importBudget{}
	}
	if budget.err != nil {
		return nil, nil, nil, noActivityColumns(), false, budget.err
	}
	missing := noActivityColumns()
	// No-follow, non-blocking open: a symlink or FIFO swapped in after the
	// folder was listed is rejected here instead of being followed or
	// blocking this run.
	file, err := openRegularFile(path)
	if err != nil {
		return nil, nil, nil, missing, false,
			fmt.Errorf("cannot open %s (only regular CSV files are imported): %w", path, err)
	}
	// The handle decides, not the path: whatever the name points at now,
	// only a regular file within the per-file cap is read.
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, nil, nil, missing, false, fmt.Errorf("cannot inspect %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, nil, nil, missing, false,
			fmt.Errorf("%s is a %s; export candidates must be regular CSV files", path, fileKind(info.Mode()))
	}
	if info.Size() > maxExportFileBytes {
		file.Close()
		return nil, nil, nil, missing, false, budget.fail(
			exportLimit("%s is larger than %d bytes and was not imported", path, maxExportFileBytes))
	}
	source := &cappedReader{
		source:    file,
		path:      path,
		remaining: maxExportFileBytes + 1,
		budget:    budget,
	}
	// A UTF-8 BOM would become part of the first header name and hide the
	// date column, so it is stripped before the header is read.
	buffered := bufio.NewReader(source)
	if prefix, err := buffered.Peek(3); err == nil && bytes.Equal(prefix, []byte{0xEF, 0xBB, 0xBF}) {
		_, _ = buffered.Discard(3)
	}
	reader := csv.NewReader(buffered)
	header, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return file, reader, nil, missing, false, nil // empty file: ignored
	}
	if err != nil {
		file.Close()
		if budget.err != nil {
			// A limit stopped the read (aggregate bytes, per-file cap): it
			// aborts the whole run instead of being mistaken for a file
			// that merely is not activity data.
			return nil, nil, nil, missing, false, budget.err
		}
		// Not parseable as CSV at all (a JSON blob, a stray quote in the
		// header line): it cannot be recognized as activity data, so it is
		// ignored like any other unrelated root CSV. Only file access
		// problems are reported as errors.
		return nil, nil, nil, missing, false, nil
	}
	cols, ok := matchActivityHeader(header)
	return file, reader, header, cols, ok, nil
}

// matchActivityHeader reports whether header identifies a date column and at
// least one activity metric column, and where each understood column is.
func matchActivityHeader(header []string) (activityColumns, bool) {
	lower := make([]string, len(header))
	for i, cell := range header {
		lower[i] = strings.ToLower(strings.TrimSpace(cell))
	}
	match := func(aliases []string) int {
		for _, alias := range aliases {
			for i, cell := range lower {
				if cell == alias {
					return i
				}
			}
		}
		return -1
	}
	cols := noActivityColumns()
	cols.date = match(dateColumnAliases)
	cols.steps = match(stepsColumnAliases)
	cols.distance = match(distanceColumnAliases)
	cols.kcal = match(kcalColumnAliases)
	// The unit heuristic only applies to an ambiguous column; an alias that
	// already says metres is taken literally.
	cols.distanceInMeters = cols.distance >= 0 &&
		(lower[cols.distance] == "distance_m" || lower[cols.distance] == "distance_meters")
	ok := cols.date >= 0 && (cols.steps >= 0 || cols.distance >= 0 || cols.kcal >= 0)
	return cols, ok
}

// recognizeActivityFile reports whether path's header identifies a date column
// and at least one activity metric column. It reads only what is needed to
// decide (through the same bounded, no-follow open as a real import), and
// never writes.
func recognizeActivityFile(path string, budget *importBudget) (bool, error) {
	file, _, _, _, ok, err := openActivityCSV(path, budget)
	if file != nil {
		file.Close()
	}
	return ok, err
}

// parseActivityFile reads one candidate CSV and normalizes its rows.
//
// Preconditions: path came from discoverActivityCSVs (a bounded, regular
// file directly inside the export folder or one of its activity subfolders)
// and budget is the run's import budget.
//
// Postconditions: recognized=false means the header does not identify a date
// and an activity metric, the file yields no records and no error. With
// recognized=true and a nil error every row has a canonical date and metrics
// inside the sanity bounds, the run's byte and record budgets cover exactly
// the rows returned, and budget.err is still nil. Any malformed row or
// exceeded limit fails with the file, the row or count and the column or
// limit — never a health value and never a silent zero — so the caller can
// abort before the database is touched.
func parseActivityFile(path string, budget *importBudget) ([]activityRecord, bool, error) {
	if budget == nil {
		budget = &importBudget{}
	}
	file, reader, header, cols, recognized, err := openActivityCSV(path, budget)
	if err != nil {
		return nil, false, err
	}
	if !recognized {
		if file != nil {
			_ = file.Close()
		}
		return nil, false, nil
	}
	defer file.Close()
	// csv.Reader has already bound the field count to the header, so every
	// record below has exactly len(header) fields.
	//
	// Invariant of the walk: after k parsed rows the budget holds exactly
	// the count of [0, k) (budget.records, bounded by maxExportRecords) and
	// budget.bytes covers every byte read so far; each iteration either
	// grows both by one bounded amount or returns an error, so the walk
	// terminates after at most min(rows in file, maxExportRecords) steps.
	var records []activityRecord
	row := 1 // the header is row 1
	for {
		row++
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if budget.err != nil {
				// A limit stopped the read (aggregate bytes, per-file cap).
				return nil, true, budget.err
			}
			return nil, true, fmt.Errorf("%s: row %d: %v", path, row, err)
		}
		record, err := rowToActivity(header, cols, fields)
		if err != nil {
			return nil, true, fmt.Errorf("%s: row %d: %w", path, row, err)
		}
		// Bound the retained slice before appending: the record budget is
		// checked on every row, so [0, k) never exceeds maxExportRecords.
		if err := budget.addRecord(path); err != nil {
			return nil, true, err
		}
		records = append(records, record)
	}
	return records, true, nil
}

// rowToActivity converts one CSV record into a normalized record.
//
// Preconditions: cols was matched against the header this record came from.
// Postconditions: a canonical date and in-range metrics, or an error naming
// the column. Cell values never appear in an error: they are health data.
func rowToActivity(header []string, cols activityColumns, fields []string) (activityRecord, error) {
	var record activityRecord
	at := func(column int) (string, error) {
		if column < 0 || column >= len(fields) {
			return "", fmt.Errorf("the row does not contain the expected columns")
		}
		return fields[column], nil
	}

	rawDate, err := at(cols.date)
	if err != nil {
		return record, fmt.Errorf("column %q: %w", header[cols.date], err)
	}
	date, err := normalizeDate(rawDate)
	if err != nil {
		return record, fmt.Errorf("column %q: %v", header[cols.date], err)
	}
	record.date = date

	if cols.steps >= 0 {
		raw, err := at(cols.steps)
		if err != nil {
			return record, fmt.Errorf("column %q: %w", header[cols.steps], err)
		}
		steps, err := parseActivitySteps(raw, header[cols.steps])
		if err != nil {
			return record, err
		}
		record.steps = steps
	}
	if cols.distance >= 0 {
		raw, err := at(cols.distance)
		if err != nil {
			return record, fmt.Errorf("column %q: %w", header[cols.distance], err)
		}
		distance, err := parseActivityDistance(raw, header[cols.distance], cols.distanceInMeters)
		if err != nil {
			return record, err
		}
		record.distanceM = distance
	}
	if cols.kcal >= 0 {
		raw, err := at(cols.kcal)
		if err != nil {
			return record, fmt.Errorf("column %q: %w", header[cols.kcal], err)
		}
		kcal, err := parseActivityKcal(raw, header[cols.kcal])
		if err != nil {
			return record, err
		}
		record.activeKcal = kcal
	}
	return record, nil
}

// normalizeDate converts one date cell to canonical YYYY-MM-DD. It accepts
// only the formats in dateLayouts — no slicing of unknown strings the way the
// reference parser does, because a mangled date must be reported, not stored.
func normalizeDate(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("the date is empty")
	}
	for _, layout := range dateLayouts {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed.Format(dateLayout), nil
		}
	}
	return "", errors.New("not a recognized date")
}

// parseActivityNumber parses one metric cell. An empty cell means the metric
// is not recorded for that row (the reference parser behaves the same way);
// any other value must be a finite, non-negative number. A malformed value is
// an error and is never stored as a silent zero.
func parseActivityNumber(raw, column string) (float64, bool, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, false, nil
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false, fmt.Errorf("column %q: not a number", column)
	}
	if parsed < 0 {
		return 0, false, fmt.Errorf("column %q: negative value", column)
	}
	return parsed, true, nil
}

// parseActivitySteps reads the steps cell and truncates it to an integer,
// like the reference parser's int(float(value)).
func parseActivitySteps(raw, column string) (int64, error) {
	value, present, err := parseActivityNumber(raw, column)
	if err != nil || !present {
		return 0, err
	}
	if value > maxActivitySteps {
		return 0, fmt.Errorf("column %q: value out of range", column)
	}
	return int64(value), nil
}

// parseActivityDistance reads the distance cell. An alias that says metres is
// taken literally; an ambiguous column (distance, dist) keeps the reference
// heuristic: a value below 100 is read as kilometres.
func parseActivityDistance(raw, column string, inMeters bool) (float64, error) {
	value, present, err := parseActivityNumber(raw, column)
	if err != nil || !present {
		return 0, err
	}
	if !inMeters && value < 100 {
		value *= 1000
	}
	if value > maxActivityDistM {
		return 0, fmt.Errorf("column %q: value out of range", column)
	}
	return value, nil
}

func parseActivityKcal(raw, column string) (float64, error) {
	value, present, err := parseActivityNumber(raw, column)
	if err != nil || !present {
		return 0, err
	}
	if value > maxActivityKcal {
		return 0, fmt.Errorf("column %q: value out of range", column)
	}
	return value, nil
}

// exportUserID derives the profile id of an export folder: export_ plus the
// SHA-256 of the absolute folder path. The configured path alone is hashed,
// so `daily` keeps resolving the same profile even after the folder is gone.
func exportUserID(exportDir string) string {
	sum := sha256.Sum256([]byte(exportDir))
	return "export_" + hex.EncodeToString(sum[:])
}
