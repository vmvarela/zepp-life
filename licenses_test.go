package main

import (
	"os"
	"strings"
	"testing"
)

// The embedded texts must be exactly the files in the repository: a binary
// built from this tree cannot carry notices that differ from what is
// published next to it.
func TestEmbeddedLicenseTextsMatchTheFiles(t *testing.T) {
	for _, file := range []string{"LICENSE", "THIRD_PARTY_NOTICES.txt"} {
		onDisk, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var got string
		switch file {
		case "LICENSE":
			got = projectLicense
		case "THIRD_PARTY_NOTICES.txt":
			got = thirdPartyNotices
		}
		if got != string(onDisk) {
			t.Errorf("embedded %s differs from the file on disk (%d vs %d bytes)",
				file, len(got), len(onDisk))
		}
	}
}

func TestLicensesPrintsProjectLicenseAndNotices(t *testing.T) {
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)

	var stdout, stderr strings.Builder
	code := run([]string{"licenses"}, &stdout, &stderr, env)
	if code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	out := stdout.String()
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}

	// The project license comes first...
	if !strings.HasPrefix(out, strings.TrimRight(projectLicense, "\n")) {
		t.Errorf("output does not start with the project license:\n%.200s", out)
	}
	// ...then the third-party notices.
	if !strings.Contains(out, "THIRD-PARTY NOTICES") {
		t.Error("output does not contain the third-party notices")
	}
	// Both are really the embedded texts, not a summary of them.
	if !strings.Contains(out, thirdPartyNotices) {
		t.Error("output does not contain the full embedded notices")
	}
	for _, want := range []string{
		"MIT License",
		"Copyright (c) 2026 Victor M. Varela",
		"github.com/zalando/go-keyring v0.2.8",
		"modernc.org/sqlite v1.62.0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q", want)
		}
	}
	// Nothing outside the two texts is printed.
	if got := strings.Count(out, "THIRD-PARTY NOTICES"); got != 1 {
		t.Errorf("notices header appears %d times, want 1", got)
	}
}

func TestLicensesTouchesNoLocalState(t *testing.T) {
	env := newTestEnv(t)
	forbidLocalAccess(t, &env)

	var stdout, stderr strings.Builder
	if code := run([]string{"licenses"}, &stdout, &stderr, env); code != exitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
	}
	if _, err := os.Stat(env.configPath); !os.IsNotExist(err) {
		t.Errorf("licenses created a configuration (stat err = %v)", err)
	}
	if _, err := os.Stat(env.databasePath); !os.IsNotExist(err) {
		t.Errorf("licenses created a database (stat err = %v)", err)
	}
	if stdout.Len() == 0 {
		t.Fatal("no output")
	}
}

func TestLicensesHelpAndArgumentHandling(t *testing.T) {
	t.Run("help exits zero and prints usage", func(t *testing.T) {
		var stdout, stderr strings.Builder
		if code := run([]string{"licenses", "--help"}, &stdout, &stderr, newTestEnv(t)); code != exitOK {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitOK, stderr.String())
		}
		if !strings.Contains(stdout.String(), "zepp-life licenses") {
			t.Errorf("help = %q", stdout.String())
		}
	})

	t.Run("unexpected argument is usage", func(t *testing.T) {
		var stdout, stderr strings.Builder
		if code := run([]string{"licenses", "extra"}, &stdout, &stderr, newTestEnv(t)); code != exitUsage {
			t.Fatalf("exit = %d, want %d (stdout: %s)", code, exitUsage, stdout.String())
		}
		if !strings.Contains(stderr.String(), "unexpected argument") {
			t.Errorf("stderr = %q", stderr.String())
		}
	})

	t.Run("unknown flag is usage", func(t *testing.T) {
		var stdout, stderr strings.Builder
		if code := run([]string{"licenses", "--nope"}, &stdout, &stderr, newTestEnv(t)); code != exitUsage {
			t.Fatalf("exit = %d, want %d (stderr: %s)", code, exitUsage, stderr.String())
		}
	})
}

// --json is a report flag; licenses prints a document, so it is not part of
// the contract. This pins the behaviour rather than leaving it accidental.
func TestLicensesIsNotAJSONReport(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := run([]string{"licenses", "--json"}, &stdout, &stderr, newTestEnv(t)); code != exitUsage {
		t.Fatalf("exit = %d, want %d (stdout: %q)", code, exitUsage, stdout.String())
	}
}
