package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestNoArgsPrintsUsageAndExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run(nil, &stdout, &stderr, newTestEnv(t))
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "Usage:") {
		t.Fatalf("stderr does not show usage: %q", stderr.String())
	}
}

func TestVersionFlag(t *testing.T) {
	saved := version
	version = "1.2.3"
	t.Cleanup(func() { version = saved })

	var stdout, stderr bytes.Buffer
	code := run([]string{"--version"}, &stdout, &stderr, newTestEnv(t))
	if code != exitOK {
		t.Fatalf("exit = %d, want %d", code, exitOK)
	}
	if got, want := stdout.String(), "zepp-life 1.2.3\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
}

func TestUnknownCommandExitsTwo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"teleport"}, &stdout, &stderr, newTestEnv(t))
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), `unknown command "teleport"`) {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestHelpListsOnlyAvailableCommands(t *testing.T) {
	for _, flagName := range []string{"--help", "-h"} {
		var stdout, stderr bytes.Buffer
		code := run([]string{flagName}, &stdout, &stderr, newTestEnv(t))
		if code != exitOK {
			t.Fatalf("%s exit = %d, want %d", flagName, code, exitOK)
		}
		help := stdout.String()
		for _, available := range []string{"setup", "sync", "doctor", "body", "daily", "licenses"} {
			if !strings.Contains(help, available) {
				t.Fatalf("%s help does not list the available command %q: %q", flagName, available, help)
			}
		}
		for _, missing := range []string{"serve", "migration"} {
			if strings.Contains(help, missing) {
				t.Fatalf("%s help mentions %q, which does not exist: %q", flagName, missing, help)
			}
		}
	}
}

func TestSubcommandHelpIsAvailableAndQuiet(t *testing.T) {
	for _, args := range [][]string{
		{"setup", "--help"},
		{"sync", "--help"},
		{"doctor", "--help"},
		{"body", "--help"},
		{"daily", "--help"},
		{"licenses", "--help"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr, newTestEnv(t))
		if code != exitOK {
			t.Fatalf("%v exit = %d, want %d", args, code, exitOK)
		}
		if stdout.Len() == 0 {
			t.Fatalf("%v printed no help", args)
		}
	}
}

func TestBodyRequiresLatestOnly(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"body"}, &stdout, &stderr, newTestEnv(t))
	if code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr.String(), "--latest-only") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestUnexpectedArgumentsAreRejected(t *testing.T) {
	cases := [][]string{
		{"doctor", "extra"},
		{"body", "--latest-only", "extra"},
		{"doctor", "--nope"},
	}
	for _, args := range cases {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr, newTestEnv(t))
		if code != exitUsage {
			t.Fatalf("%v exit = %d, want %d", args, code, exitUsage)
		}
	}
}

func TestJSONFlagWorksBeforeAndAfterTheCommand(t *testing.T) {
	for _, args := range [][]string{{"--json", "doctor"}, {"doctor", "--json"}} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr, newTestEnv(t))
		if code != exitOK {
			t.Fatalf("%v exit = %d, want %d", args, code, exitOK)
		}
		var report doctorReport
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("%v output is not JSON: %v\n%s", args, err, stdout.String())
		}
		if report.Command != "doctor" {
			t.Fatalf("%v command = %q", args, report.Command)
		}
	}
}
