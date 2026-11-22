// Command zepp-life configures cloud or export sources, syncs daily activity
// or body measurements, and queries the local SQLite cache.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
)

const (
	exitOK    = 0 // checks passed, or there is simply no configuration/data yet
	exitError = 1 // operational problem: invalid configuration, unreadable database
	exitUsage = 2 // invalid command line usage
)

// version is the release version, injected by the release workflow with
// -ldflags "-X main.version=X.Y.Z"; untagged builds report "dev".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, defaultEnvironment()))
}

func run(args []string, stdout, stderr io.Writer, env environment) int {
	root := flag.NewFlagSet(programName, flag.ContinueOnError)
	root.SetOutput(io.Discard)
	root.Usage = func() {}
	globalJSON := root.Bool("json", false, "print JSON instead of human-readable text")
	showVersion := root.Bool("version", false, "print the version and exit")
	if err := root.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printUsage(stdout)
			return exitOK
		}
		fmt.Fprintf(stderr, "%s: %v\n\n", programName, err)
		printUsage(stderr)
		return exitUsage
	}
	if *showVersion {
		fmt.Fprintf(stdout, "%s %s\n", programName, version)
		return exitOK
	}

	rest := root.Args()
	if len(rest) == 0 {
		printUsage(stderr)
		return exitUsage
	}
	name, cmdArgs := rest[0], rest[1:]
	switch name {
	case "setup":
		return runSetup(cmdArgs, *globalJSON, stdout, stderr, env)
	case "sync":
		return runSync(cmdArgs, *globalJSON, stdout, stderr, env)
	case "doctor":
		return runDoctor(cmdArgs, *globalJSON, stdout, stderr, env)
	case "body":
		return runBody(cmdArgs, *globalJSON, stdout, stderr, env)
	case "daily":
		return runDaily(cmdArgs, *globalJSON, stdout, stderr, env)
	case "licenses":
		return runLicenses(cmdArgs, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "%s: unknown command %q\n\n", programName, name)
		printUsage(stderr)
		return exitUsage
	}
}

// parseCommandFlags parses a subcommand flag set. It reports whether help was
// requested and, when the command line is invalid, the exit code to use.
func parseCommandFlags(fs *flag.FlagSet, args []string, usage func(io.Writer), stdout, stderr io.Writer) (help bool, code int) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			usage(stdout)
			return true, exitOK
		}
		fmt.Fprintf(stderr, "%s %s: %v\n\n", programName, fs.Name(), err)
		usage(stderr)
		return false, exitUsage
	}
	return false, 0
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `zepp-life - store your Zepp Life data locally and inspect it

Usage:
  zepp-life setup [--user-id ID] [--replace-token] [--json]
  zepp-life setup --source export-file --export-dir DIR [--json]
  zepp-life sync --type body_measurements [--start-date YYYY-MM-DD] [--end-date YYYY-MM-DD] [--json]
  zepp-life sync --type daily_activity [--json]
  zepp-life daily --date YYYY-MM-DD [--json]
  zepp-life doctor [--json]
  zepp-life body --latest-only [--json]
  zepp-life licenses

Commands:
  setup     Store your Zepp cloud session, or configure an export folder
  sync      Download body measurements or daily activity from the Zepp
            cloud, or import daily activity from the export folder — the
            source of daily_activity follows the configuration
  daily     Show one day of cached daily activity, per device
  doctor    Check configuration, export folder, database and credentials
  body      Show body measurements from the local database
  licenses  Print the project license and the third-party notices

Global flags:
  --json      Print JSON instead of human-readable text
  --version   Print the version and exit

The source of daily_activity follows the configured mode, with no fallback:
cloud_session fetches it from the Zepp cloud (stored token), export_file
imports it offline from the export folder without touching the keyring.
body_measurements always comes from the Zepp cloud. sync never prompts.
doctor, daily and body only read local files and never use the network;
licenses reads nothing at all — both texts are embedded in the binary.

Exit codes:
  0  everything worked, or there is simply nothing configured yet (both normal)
  1  operational problem: invalid configuration, rejected token, unreadable database
  2  invalid command line usage
`)
}

func printDoctorUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  zepp-life doctor [--json]

Checks your configuration, your export folder (in export_file mode), your
local database and your stored credentials. In export_file mode the keyring
is not read at all and reported as skipped.
Read-only: nothing is created, changed or sent anywhere.

Flags:
  --json    Print JSON instead of human-readable text
`)
}

func printBodyUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  zepp-life body --latest-only [--json]

Prints your most recent body measurement from the local database.
Read-only: nothing is created, changed or sent anywhere.

Flags:
  --latest-only   Show only the most recent measurement (required)
  --json          Print JSON instead of human-readable text
`)
}

// emitReport writes the result of a command as JSON or as human-readable text.
func emitReport(w io.Writer, jsonOut bool, report any, human func(io.Writer)) {
	if jsonOut {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
		return
	}
	human(w)
}
