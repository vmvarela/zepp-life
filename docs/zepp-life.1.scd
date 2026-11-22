zepp-life(1)

NAME
	zepp-life - store your Zepp Life data locally and inspect it

SYNOPSIS
	*zepp-life* *setup* [_options_]
	*zepp-life* *setup* *--source* _export-file_ *--export-dir* _DIR_ [_options_]
	*zepp-life* *sync* *--type* _body_measurements_ [_options_]
	*zepp-life* *sync* *--type* _daily_activity_ [_options_]
	*zepp-life* *daily* *--date* _YYYY-MM-DD_ [_options_]
	*zepp-life* *doctor* [_options_]
	*zepp-life* *body* *--latest-only* [_options_]
	*zepp-life* *licenses*
	*zepp-life* *--version*
	*zepp-life* *--help*

DESCRIPTION
	*zepp-life* is a standalone command line tool for your Zepp Life data. It
	syncs daily activity from the Zepp cloud or imports it from an export
	folder, downloads body measurements into a local SQLite database, and
	lets you inspect what is stored. No Python runtime or server is required.

	The source of daily activity is chosen by *setup* alone and *sync* never
	falls back between them: *cloud_session* fetches from the Zepp cloud with
	the stored token, *export_file* re-reads the export folder offline. The
	export commands (*setup --source export-file*, *sync --type
	daily_activity* in *export_file* mode, *daily*, *doctor*) never touch the
	keyring and never make a request.

	Credentials are stored in the system keyring, never in the configuration
	file and never on the command line. *doctor*, *daily* and *body* are
	read-only: they never create or change configuration or database state.
	*licenses* reads nothing at all: both texts it prints are embedded in
	the binary.

COMMANDS
	*setup* [--user-id _ID_] [--replace-token] [--source _cloud-session_|_export-file_] [--export-dir _DIR_] [--json]
		Stores your Zepp cloud session (token and user id), read from a
		hidden terminal prompt. A saved session that is already complete is
		reused without asking again.

		With *--source export-file* it instead validates that _DIR_ exists
		and holds recognizable activity CSVs, then stores its absolute path
		as *export_path* and switches the mode, preserving every other
		configuration field. This branch never reads the keyring, never
		prompts and never uses the network.

	*sync* *--type* _body_measurements_ [--start-date _YYYY-MM-DD_] [--end-date _YYYY-MM-DD_] [--json]
		Downloads body measurements from the Zepp cloud into the local
		database. Never prompts; at most 200 records per run.

	*sync* *--type* _daily_activity_ [--start-date _YYYY-MM-DD_] [--end-date _YYYY-MM-DD_] [--json]
		Uses the configured source with no fallback. In *cloud_session*
		mode it fetches daily summaries from the Zepp cloud in one bounded
		request (the date flags apply). In *export_file* mode it re-reads
		the whole export folder offline in one atomic transaction and
		rejects the date flags. If any row is malformed nothing is written
		and the error names the file, row and column.

	*daily* *--date* _YYYY-MM-DD_ [--json]
		Prints the cached activity of one day, one block per device.
		Devices are never summed. Read-only, offline; a date with no data
		exits 0 with status *no_data*.

	*doctor* [--json]
		Checks configuration, export folder, database and stored
		credentials. Read-only; in *export_file* mode the keyring check is
		reported as *skipped* and the keyring is not read.

	*body* *--latest-only* [--json]
		Prints the most recent body measurement from the local database.
		*--latest-only* is required. Read-only.

	*licenses*
		Prints this program's MIT license followed by the third-party
		notices of every module linked into the binary. Both texts are
		embedded, so a raw-binary install (Scoop, Nix, WinGet, a bare
		release asset) can show them without any companion file.
		Read-only: no configuration, database, keyring or network is
		touched. It prints a document, not a report, so *--json* does
		not apply.

OPTIONS
	*--json*
		Print JSON instead of human-readable text. Accepted globally
		(before the command) and by every command.

	*--version*
		Print the version and exit with status 0. Release builds carry
		the tag version; an untagged build reports *dev*.

	*-h*, *--help*
		Print the usage summary and exit with status 0.

EXIT CODES
	*0*
		Everything worked, or there is simply nothing configured yet, or
		no data exists for that date. All three are normal.

	*1*
		Operational problem: invalid configuration, rejected token,
		missing setup, unreadable or unexpected database, ambiguous
		profile, malformed export row.

	*2*
		Invalid command line usage: unknown command, unknown flag, bad
		date, unsupported *--type* or *--source*.

FILES
	Configuration, one file per operating system:

		Linux     ~/.config/zepp-life-mcp/config.json
		(or $XDG_CONFIG_HOME/zepp-life-mcp/config.json)
		macOS     ~/Library/Application Support/zepp-life-mcp/config.json
		Windows   %LOCALAPPDATA%\\zepp-life-mcp\\zepp-life-mcp\\config.json

	*setup* writes only the fields it owns (*mode*, and *export_path* in
	export mode) and keeps every other field of an existing file exactly as
	it is.

	Database, one file per operating system:

		Linux     ~/.local/share/zepp-life-mcp/zepp_life.db
		(or $XDG_DATA_HOME/zepp-life-mcp/zepp_life.db)
		macOS     ~/Library/Application Support/zepp-life-mcp/zepp_life.db
		Windows   %LOCALAPPDATA%\\zepp-life-mcp\\zepp-life-mcp\\zepp_life.db

	*sync* creates a missing database with the complete version 6 schema and
	reports (never repairs) an existing database of a different version.
	*daily* and *body* open it read-only and never create it. A
	*database_path* in the configuration points the tool somewhere else.

	Credentials live in the system keyring under the service
	*zepp-life-mcp*, with the entries *zepp_auth_token* and
	*zepp_auth_user_id*: Keychain on macOS, Secret Service on Linux and the
	Credential Manager on Windows.

EXAMPLES
	Import activity from an export folder:

		$ zepp-life setup --source export-file --export-dir ~/Downloads/zepp-export
		$ zepp-life sync --type daily_activity
		$ zepp-life daily --date 2026-11-19

	Sync activity from the Zepp cloud:

		$ zepp-life setup
		$ zepp-life sync --type daily_activity

	Sync body measurements and inspect the latest one:

		$ zepp-life sync --type body_measurements
		$ zepp-life body --latest-only

	Check everything without changing anything:

		$ zepp-life doctor

	Print the license and the third-party notices:

		$ zepp-life licenses

	Print a machine-readable report:

		$ zepp-life daily --date 2026-11-19 --json

AUTHORS
	Victor Varela <https://github.com/vmvarela>

SEE ALSO
	For more information, visit https://github.com/vmvarela/zepp-life
