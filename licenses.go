package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"strings"
)

// The two texts travel inside every binary: a raw-binary install (Scoop, Nix,
// WinGet, a bare release asset) has no LICENSE file next to it, so the
// notices have to be reachable from the executable itself.
//
//go:embed LICENSE
var projectLicense string

//go:embed THIRD_PARTY_NOTICES.txt
var thirdPartyNotices string

// licensesText is the whole document `zepp-life licenses` prints: the project
// license first, then the third-party notices, separated by a blank line.
func licensesText() string {
	return strings.TrimRight(projectLicense, "\n") + "\n\n" + thirdPartyNotices
}

// runLicenses prints the project license and the third-party notices. It is
// read-only in the strongest sense: no configuration, database, keyring or
// network is touched, because both texts are embedded in the binary. Like
// --version it is a document, not a report, so --json does not apply.
func runLicenses(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("licenses", flag.ContinueOnError)
	if help, code := parseCommandFlags(flags, args, printLicensesUsage, stdout, stderr); help || code != 0 {
		return code
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s licenses: unexpected argument %q\n\n", programName, flags.Arg(0))
		printLicensesUsage(stderr)
		return exitUsage
	}
	fmt.Fprint(stdout, licensesText())
	return exitOK
}

func printLicensesUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  zepp-life licenses

Prints this program's MIT license followed by the third-party notices of
every module linked into the binary. Both texts are embedded, so a
raw-binary install can show them without any companion file.

Read-only: nothing is created, changed or sent anywhere, and neither the
configuration, the database, the keyring nor the network is used.

Flags:
  -h, --help  Print this help and exit
`)
}
