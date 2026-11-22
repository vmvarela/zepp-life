//go:build unix

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runWithin runs fn on a goroutine and fails the test if it does not return
// within d. A special file that blocks on open would otherwise hang the
// whole suite instead of failing it.
func runWithin(t *testing.T, d time.Duration, fn func() int) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- fn() }()
	select {
	case code := <-done:
		return code
	case <-time.After(d):
		t.Fatalf("command did not finish within %v; it would block on a special file", d)
		return -1
	}
}

// --- symlink to /dev/zero --------------------------------------------------

// A symlink to an endless device must be rejected by name while the folder
// is listed: nothing is ever opened, so the run cannot read without bound.
func TestSetupAndSyncRejectSymlinkToDevZero(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("/dev/zero unavailable: %v", err)
	}
	dir := newExportDir(t, map[string]string{"ACTIVITY/real.csv": activityTestCSV})
	link := filepath.Join(dir, "ACTIVITY", "zero.csv")
	if err := os.Symlink("/dev/zero", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	t.Run("setup", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeValidConfig(t, env.configPath, env.databasePath)
		before := snapshotConfig(t, env.configPath)

		var out string
		code := runWithin(t, 10*time.Second, func() int {
			var stdout, stderr strings.Builder
			c := run([]string{"setup", "--source", srcExportFile, "--export-dir", dir, "--json"},
				&stdout, &stderr, env)
			out = stdout.String()
			return c
		})
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stdout: %s)", code, exitError, out)
		}
		var report setupReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, "symlink") || !strings.Contains(report.Detail, link) {
			t.Errorf("detail = %q, want the symlink report", report.Detail)
		}
		if after := snapshotConfig(t, env.configPath); after != before {
			t.Fatalf("config changed after a failed setup:\nbefore %q\nafter  %q", before, after)
		}
	})

	t.Run("sync", func(t *testing.T) {
		env := newTestEnv(t)
		forbidLocalAccess(t, &env)
		writeExportConfig(t, env.configPath, env.databasePath, dir)

		var out string
		code := runWithin(t, 10*time.Second, func() int {
			var stdout, stderr strings.Builder
			c := run([]string{"sync", "--type", typeDailyActivity, "--json"}, &stdout, &stderr, env)
			out = stdout.String()
			return c
		})
		if code != exitError {
			t.Fatalf("exit = %d, want %d (stdout: %s)", code, exitError, out)
		}
		var report syncReport
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(report.Detail, "symlink") {
			t.Errorf("detail = %q, want the symlink report", report.Detail)
		}
		assertSyncFailedWithoutCreatingTheDatabase(t, env)
	})
}

// --- the no-follow primitive -----------------------------------------------

func TestOpenRegularFileRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.csv")
	if err := os.WriteFile(target, []byte(activityTestCSV), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.csv")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	file, err := openRegularFile(link)
	if err == nil {
		file.Close()
		t.Fatal("openRegularFile followed a symlink; O_NOFOLLOW is not in effect")
	}
}

// A FIFO must neither block the open nor be accepted as a regular file.
func TestFIFOIsRejectedWithoutBlocking(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.csv")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("FIFOs unavailable: %v", err)
	}

	// The primitive returns immediately (O_NONBLOCK) and whatever it hands
	// back is not a regular file.
	runWithin(t, 5*time.Second, func() int {
		file, err := openRegularFile(fifo)
		if err != nil {
			return 0 // rejected at open is fine; the point is not blocking
		}
		defer file.Close()
		info, statErr := file.Stat()
		if statErr == nil && info.Mode().IsRegular() {
			t.Error("the FIFO handle was reported as a regular file")
		}
		return 0
	})

	// End to end: discovery rejects it by name, before any open.
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)
	writeExportConfig(t, env.configPath, env.databasePath, dir)
	var out string
	code := runWithin(t, 10*time.Second, func() int {
		var stdout, stderr strings.Builder
		c := run([]string{"sync", "--type", typeDailyActivity, "--json"}, &stdout, &stderr, env)
		out = stdout.String()
		return c
	})
	if code != exitError {
		t.Fatalf("exit = %d, want %d (stdout: %s)", code, exitError, out)
	}
	var report syncReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.Detail, "FIFO") || !strings.Contains(report.Detail, fifo) {
		t.Errorf("detail = %q, want the FIFO report", report.Detail)
	}
	assertSyncFailedWithoutCreatingTheDatabase(t, env)
}

// The opened-handle check rejects a device even when the preflight stat
// would have passed (a device reports size 0).
func TestOpenActivityCSVRejectsDeviceHandle(t *testing.T) {
	if _, err := os.Stat("/dev/zero"); err != nil {
		t.Skipf("/dev/zero unavailable: %v", err)
	}
	_, _, _, _, _, err := openActivityCSV("/dev/zero", &importBudget{})
	if err == nil {
		t.Fatal("a device handle was accepted as an export candidate")
	}
	if !strings.Contains(err.Error(), "device") {
		t.Errorf("error = %q, want the device kind named", err)
	}
}
