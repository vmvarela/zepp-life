# zepp-life

A small command line tool for your Zepp Life data: it syncs daily activity
from the Zepp cloud or imports it from an export folder, downloads body
measurements from the Zepp cloud into the local SQLite database, and lets
you inspect what is stored. A standalone Go CLI; no Python runtime or MCP
server is required. Windows, macOS and Linux on amd64 and arm64.

## Quick start (activity from an export folder)

```sh
go build -o zepp-life .

zepp-life setup --source export-file --export-dir ~/Downloads/zepp-export
zepp-life sync --type daily_activity
zepp-life daily --date 2026-11-19
```

`setup` checks that the folder exists and that at least one CSV in it has a
recognizable activity header, then stores the absolute path in the
configuration. `sync` re-reads the whole folder and imports it in one
transaction — fully offline: no keyring, no prompt, no network. `daily` reads
one date back from the local database, one block per device.

These three export commands never touch the keyring and never make a
request. The cloud commands do: `setup` (cloud mode),
`sync --type body_measurements` and `sync --type daily_activity` in
`cloud_session` mode.

## Daily activity from the Zepp cloud

```sh
zepp-life setup                   # a cloud session, if you do not have one
zepp-life sync --type daily_activity
zepp-life daily --date 2026-11-19
```

With a `cloud_session` configuration the same sync fetches daily summaries
from the Zepp cloud: one bounded request whose window runs from the stored
sync cursor (default `2020-01-01`) to today, with date flags to narrow it.
The source is chosen by `setup` alone — `cloud_session` goes to the cloud,
`export_file` reads the folder offline — and there is no fallback between
them. `daily` never contacts either source: it prints whatever `sync`
cached.

## Cloud body measurements

```sh
zepp-life setup                  # enter your Zepp app token (hidden prompt)
zepp-life sync --type body_measurements
zepp-life body --latest-only
```

The token is the `apptoken` cookie from <https://user.huami.com/privacy2/index.html>.
It is read from the terminal with echo turned off and stored in the system
keyring: never as a command line flag, never from piped input, never printed.

## Commands

| Command | What it does |
| --- | --- |
| `zepp-life setup --source export-file --export-dir DIR` | Validates DIR and stores its absolute path as `export_path` with mode `export_file`. Preserves every other configuration field. No keyring, no prompt, no network. |
| `zepp-life setup` | Stores your Zepp cloud session (token and user id). A saved session that is already complete is reused without asking again. Default on a clean install; an `export_file` configuration is only left when `--source cloud-session` is asked for explicitly. |
| `zepp-life sync --type daily_activity` | Uses the configured source, with no fallback: `cloud_session` fetches daily summaries from the Zepp cloud in one bounded request (date flags apply); `export_file` re-reads the export folder offline in one atomic transaction (date flags rejected, no keyring, no prompt, no network). |
| `zepp-life sync --type body_measurements` | Downloads body measurements from the Zepp cloud into the local database. Never prompts. |
| `zepp-life daily --date YYYY-MM-DD` | Prints one day of the activity `sync` cached — from the cloud or the export folder — one row per device (devices are never summed). Read-only, offline. |
| `zepp-life doctor` | Checks configuration, export folder, database and stored credentials. Read-only; the keyring is reported as `skipped` in export_file mode. |
| `zepp-life body --latest-only` | Prints your most recent body measurement. Read-only. |
| `zepp-life licenses` | Prints this program's MIT license followed by the third-party notices of every module linked into the binary. Both texts are embedded, so a raw-binary install can show them without a companion file. Reads nothing: no configuration, database, keyring or network. |

`zepp-life --help` lists exactly what exists, nothing more.

## What exists today

Two sources, three data types:

- **Daily activity from the Zepp cloud** (`daily_activity` with a
  `cloud_session` configuration): one bounded summary request per sync over
  the window from the stored cursor (default `2020-01-01`) to today. The
  cursor advances only when the whole sync commits; a failure records the
  error and leaves rows and cursor untouched.
- **Activity CSV from an export folder** (`daily_activity` with an
  `export_file` configuration), imported offline.
- **Body measurements from the Zepp cloud** (`body_measurements`), with a
  single request of at most 200 records per sync (the endpoint offers no
  pagination).

The source of `daily_activity` follows the configuration alone; rejected
with the precise next step are: `body_measurements` in `export_file` mode,
an unknown `--type`, an inverted date window, and date flags with the
export import (it always reads the whole folder).

`doctor` and `daily` only read local files and never use the network.

### The activity parser is heuristic

There is **no official, well-defined Zepp export format** for this tool to
target, and this repository contains **no real exports** to verify against.
The parser recognizes a limited set of filenames, headers and date formats:

- Only CSV files are read, non-recursively: directly inside `ACTIVITY/`,
  `activity/`, `steps/` or `STEPS/`, or in the root of the export folder.
  Root CSVs are ignored unless their header identifies a date column and at
  least one activity metric, so unrelated files are not mistaken for data.
- Header names are matched case-insensitively against aliases: date
  (`date`, `day`, `timestamp`, `time`), steps (`steps`, `step`, `step_count`,
  `steps_count`), distance (`distance`, `distance_m`, `distance_meters`,
  `dist`) and calories (`calories`, `cal`, `kcal`, `active_calories`,
  `energy`).
- Dates are normalized to `YYYY-MM-DD` from `YYYY-MM-DD`, `YYYY/MM/DD`,
  `DD.MM.YYYY`, `DD/MM/YYYY`, `MM/DD/YYYY`, `YYYY-MM-DD HH:MM:SS`, and ISO
  `T`/`Z` datetimes. Day/month is tried before month/day, so `05/11/2026`
  means 5 November. Anything else is reported as an error, never guessed.
- `distance_m` and `distance_meters` are metres. The ambiguous `distance` and
  `dist` columns use this heuristic: **a value below 100 is read as
  kilometres, otherwise as metres** — so a 90 km cycle stored as `90` becomes
  90000 m. If your export uses metres in a bare `distance` column, values
  below 100 m will be over-read.
- Steps are truncated to an integer.
- A malformed date or number in a recognized CSV fails the import: the error
  names the file, the row and the column (never the health value), and
  nothing is written. UTF-8 BOMs are handled.

Because the format is heuristic, a future export with different headers or
units may not import cleanly. The error message always says which file, row
and column to look at.

### Import limits

Every export run (`setup --source export-file` validation and
`sync --type daily_activity` in `export_file` mode) is bounded, so a
hostile or broken folder cannot read or scan without bound:

- **Regular files only.** Symlinks, FIFOs, sockets and devices are rejected
  by name while the folder is listed; the file is then opened without
  following a final symlink (Linux, macOS and the other Unix targets) and
  the opened handle must still be a regular file, so a path swapped between
  inspection and open is caught as well.
- **The fixed activity folders must be real directories.** A symlink (or a
  plain file) under `ACTIVITY/`, `activity/`, `steps/` or `STEPS/` is
  rejected instead of traversed, so those names cannot redirect the import
  to an arbitrary tree. The export root itself is your explicit choice and
  may be a symlink.
- **At most 4096 directory entries scanned** — every name, CSV or not —
  across the export root and its four known activity folders, checked while
  the listing streams in batches. A folder stuffed with unrelated files
  stops the scan at a fixed count instead of being walked to its end.
- **At most 256 candidate CSVs**, **64 MiB read in total** and **100,000
  rows parsed** per run, on top of the existing **8 MiB per file** cap.
  Real exports hold a handful of CSVs and a few thousand rows, so these caps
  sit orders of magnitude above any legitimate folder while keeping the work
  on a hostile one fixed.

Hitting any limit fails the run with the path and the counts involved — never
a cell value — before the configuration or the database is touched, so a
failed run changes nothing. On Windows there is no no-follow open: symlinks
are still rejected while the folder is listed, but one swapped in between
that check and the open would be followed (the handle must then be a regular
file).

## Examples

### Setup (export folder)

```console
$ zepp-life setup --source export-file --export-dir ~/Downloads/zepp-export
Zepp Life export setup
  export dir:    /home/ana/Downloads/zepp-export
  config:        /home/ana/.config/zepp-life-mcp/config.json

Ready. Next: zepp-life sync --type daily_activity
```

### Sync (export folder)

```console
$ zepp-life sync --type daily_activity
Imported daily activity
  profile:       export_2f8a…
  added:         30
  updated:       2
  skipped:       0
Next: zepp-life daily --date 2026-11-19
```

Counters partition the imported rows: `added` are new rows, `updated` are
rows whose values changed, `skipped` already matched. The whole import is one
transaction: if any row is malformed, nothing is saved and the error names
the file, row and column. Re-running the same folder is idempotent — the
second run reports `skipped` for everything unchanged.

```console
$ zepp-life sync --type daily_activity --json
{
  "command": "sync",
  "status": "ok",
  "type": "daily_activity",
  "user_id": "export_2f8a…",
  "start_date": "",
  "end_date": "",
  "added": 30,
  "updated": 2,
  "skipped": 0,
  "detail": "Next: zepp-life daily --date 2026-11-19"
}
```

### Sync (cloud)

```console
$ zepp-life sync --type daily_activity
Synced daily activity from the Zepp cloud
  profile:       123456789
  range:         2026-10-01 to 2026-11-21
  added:         1
  updated:       0
  skipped:       0
  Next: zepp-life daily --date 2026-11-21
```

With a cloud configuration the default window starts at the stored cursor,
so each run only asks for days it has not stored yet. Re-running with an
explicit window over days that are already stored reports
`No changes: all N daily activity rows from the Zepp cloud are already up
to date` instead of counting them as added.

### Daily

```console
$ zepp-life daily --date 2026-11-19
Daily activity 2026-11-19
  profile:       export_2f8a…
  device:        -
    steps:       10432
    distance:    7350.0 m
    active kcal: 412.0 kcal
```

Devices are printed separately and never summed: their daily windows can
overlap. In `export_file` mode the profile is derived deterministically
from the absolute `export_path`, so `daily` keeps working even after the
export folder is deleted; in `cloud_session` mode the stored profile is
used and a stale `export_path` is ignored.

```console
$ zepp-life daily --date 2026-11-19 --json
{
  "command": "daily",
  "status": "ok",
  "date": "2026-11-19",
  "user_id": "export_2f8a…",
  "records": [
    {
      "device_id": null,
      "device_key": "",
      "steps": 10432,
      "distance_m": 7350,
      "active_kcal": 412
    }
  ],
  "detail": ""
}
```

A date with no data exits `0` with `"status": "no_data"`.

### Cloud sync

```console
$ zepp-life sync --type body_measurements
Synced body measurements
  profile:       123456789
  range:         2020-01-01 to 2026-11-20
  added:         2
  updated:       0
  skipped:       1
```

If a cloud run fails, no partial measurement rows are saved: the cursor and
the last successful run stay untouched, and the error is recorded.

### Body

```console
$ zepp-life body --latest-only
Latest body measurement
  profile:         123456789
  timestamp:       2025-11-02T07:00:00+00:00
  weight:          71.5 kg
  bmi:             22.1
  body fat:        18.5 %
```

Values that are not recorded are simply left out of the human output.

`doctor` reports four checks (`config`, `export`, `database`, `keyring`),
each with a `status` and a `detail` line. In export_file mode the keyring
check is `skipped` and the keyring is not read.

### Errors

Every failure says what to do next:

```console
$ zepp-life sync --type sleep
zepp-life sync: unsupported --type "sleep"; this build supports body_measurements and daily_activity.
```

```console
$ zepp-life sync --type body_measurements   # export_file configuration
{
  "command": "sync",
  "status": "error",
  ...
  "detail": "body_measurements comes from the Zepp cloud; your configuration uses export_file mode and was not changed. Run `zepp-life sync --type daily_activity` to import the folder instead."
}
```

A rejected token suggests `zepp-life setup --replace-token`; a missing
session suggests `zepp-life setup`; an unreadable or unexpected database is
reported and left completely untouched.

## Build

```sh
go build -o zepp-life .
```

No C compiler is required: the SQLite driver is pure Go. The same command
cross-builds for all six supported targets:

```sh
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -o zepp-life-linux-amd64 .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -o zepp-life-linux-arm64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=amd64 go build -o zepp-life-darwin-amd64 .
CGO_ENABLED=0 GOOS=darwin  GOARCH=arm64 go build -o zepp-life-darwin-arm64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o zepp-life-windows-amd64.exe .
CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build -o zepp-life-windows-arm64.exe .
```

## Install

Take a prebuilt binary from a published GitHub Release, or build from source
as above. A raw binary (Scoop, Nix, WinGet, a bare release asset) arrives
with no companion files, so the license texts are embedded in it:

```console
$ zepp-life licenses
```

prints the project `LICENSE` followed by `THIRD_PARTY_NOTICES.txt`; both are
embedded in the binary, which is what makes a raw-binary install work at all.
Of the two, only `THIRD_PARTY_NOTICES.txt` is published as its own release
asset — `LICENSE` is not one of the release assets. The deb, rpm, apk, npm
and AUR packages ship both files on disk, the Homebrew formula installs the
notices file only, and the raw-binary channels ship neither, so those installs
read the texts with `zepp-life licenses`.

### Distribution channels

Pushing a strict `vX.Y.Z` tag runs the release workflow, which builds the six
binaries, the man page and the third-party notices, stages them with the deb,
rpm and apk packages as a draft release, generates `sha256sums.txt` over
exactly those 14 assets, publishes it, and only then updates the channels
below. **Every channel update therefore requires a published stable
release**: nothing is pushed to a channel from a draft, from an untagged
build or from a pre-release tag. The table describes what each channel
receives once its prerequisite is in place — it does not report which
channels have actually published.

| Channel | Target | One-time prerequisite |
| --- | --- | --- |
| GitHub Releases | six binaries, `zepp-life.1.gz`, `THIRD_PARTY_NOTICES.txt`, deb/rpm/apk, `sha256sums.txt` | none — every strict `vX.Y.Z` tag |
| npm | package `@vmvarela/zepp-life` wrapping the six binaries, with `LICENSE` and `THIRD_PARTY_NOTICES.txt` | claim the package name on npmjs.com, then configure **exactly one** of two mutually exclusive paths: set the repository variable `NPM_TRUSTED_PUBLISHING=true` to publish by OIDC trusted publishing (no token, `--provenance` on), **or** leave that variable unset and add the `NPM_TOKEN` secret to publish by token (no OIDC, no provenance). With neither configured both npm jobs no-op and the release still ships. |
| Homebrew | formula `zepp-life.rb` in `vmvarela/homebrew-tap`, installing the binary, the man page and the notices | the tap repository with a `Formula/` directory, plus the `PACKAGE_DISTRIBUTION_TOKEN` secret (fine-grained PAT with `contents: write` on that tap) |
| AUR | package `zepp-life`, installing the binary, the man page, `LICENSE` and `THIRD_PARTY_NOTICES.txt` | the package registered at <https://aur.archlinux.org/packages/zepp-life>, plus the `PACKAGE_SSH_KEY` secret (SSH key authorized on AUR) |
| WinGet | manifest updates for `vmvarela.zepp-life` in `microsoft/winget-pkgs` | **the first version must be registered by hand** (wingetcreate or a manual PR) — the workflow only refreshes a manifest that already exists. It also needs a fork of `microsoft/winget-pkgs` under your account and the `PACKAGE_DISTRIBUTION_TOKEN` secret |
| repository dispatch | `update-zepp-life` events to `vmvarela/apt-packages`, `rpm-packages`, `apk-packages`, `nix-packages` and `scoop-bucket`, each of which re-reads `catalog/zepp-life.json` for the version | the `PACKAGE_DISTRIBUTION_TOKEN` secret, allowed to dispatch to those repositories |

A missing secret or variable never fails a release: that channel's job is
skipped instead, and the GitHub Release is published either way. So whether a
channel is live depends on its prerequisite being configured, not on the
workflow alone.

**npm has a registry-side prerequisite that no repository setting can
replace.** Trusted publishing is configured on npmjs.com: the package
`@vmvarela/zepp-life` must name this GitHub repository (`vmvarela/zepp-life`)
and this workflow file (`release.yml`) as a trusted publisher. The repository
variable `NPM_TRUSTED_PUBLISHING=true` only tells the workflow to ask for an
OIDC token — npm rejects the publish unless the registry itself trusts the
caller. The `NPM_TOKEN` secret is the **fallback** path: it is read only when
that variable is unset, and it publishes without OIDC and without
`--provenance`. The two paths are mutually exclusive, and with neither
configured both npm jobs no-op.

### Rollout order for the dispatch receivers

The five receivers (`vmvarela/apt-packages`, `rpm-packages`, `apk-packages`,
`nix-packages` and `scoop-bucket`) each pin `PACKAGE_DISTRIBUTION_SHA` to a
merge commit of the package-distribution repository, and each runs on a
schedule as well as on the `update-zepp-life` dispatch. This repository does
not set those pins and does not report their state. The intended order is:

1. Publish the first public stable `vX.Y.Z` release.
2. Update the five `PACKAGE_DISTRIBUTION_SHA` pins to that package-distribution
   merge commit.
3. Activate the dispatch/receiver integration.

That order matters because a receiver schedule must never read
`catalog/zepp-life.json` before a stable release exists for it to point at.

## Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Everything worked, or there is simply nothing configured yet / no data for that date. Both are normal. |
| `1` | Operational problem: invalid configuration, rejected token, missing setup, unreadable or unexpected database, ambiguous profile, malformed export row. |
| `2` | Invalid command line usage: unknown command, unknown flag, bad date, unsupported `--type` or `--source`. |

## Where it reads and writes

The locations are fixed and shared with the same local data layout on every
platform, so an existing setup keeps working as it is.

### Configuration

| Operating system | Configuration file |
| --- | --- |
| Linux | `~/.config/zepp-life-mcp/config.json` (or `$XDG_CONFIG_HOME/zepp-life-mcp/config.json`) |
| macOS | `~/Library/Application Support/zepp-life-mcp/config.json` |
| Windows | `%LOCALAPPDATA%\zepp-life-mcp\zepp-life-mcp\config.json` |

`setup` writes only the fields it owns (`mode`, and `export_path` in export
mode) and keeps every other field of an existing file exactly as it is.
`doctor`, `daily` and `body` never write it.

### Database

| Operating system | Database file |
| --- | --- |
| Linux | `~/.local/share/zepp-life-mcp/zepp_life.db` (or `$XDG_DATA_HOME/zepp-life-mcp/zepp_life.db`) |
| macOS | `~/Library/Application Support/zepp-life-mcp/zepp_life.db` |
| Windows | `%LOCALAPPDATA%\zepp-life-mcp\zepp-life-mcp\zepp_life.db` |

If `database_path` in the configuration points somewhere else, that path is
used instead. `sync` creates a missing database with the complete version 6
schema (all six tables and indexes) and `user_version = 6`. An existing
database with a different version or an incomplete version 6 schema is
reported clearly and left completely untouched. `daily` and `body` open it
read-only and never create or modify it.

### Credentials

Service `zepp-life-mcp` in the system keyring, with the entries
`zepp_auth_token` and `zepp_auth_user_id`:

- **macOS**: Keychain.
- **Linux**: Secret Service (the desktop keyring).
- **Windows**: Credential Manager, with the same target and user name layout
  the keyring library uses, so credentials stay interchangeable.

`setup` writes raw values that any keyring reader can open; no secret is ever
printed, logged or included in JSON output. `doctor` only reports whether each
entry is stored, and does not read the keyring at all in export_file mode.
The export workflow (`setup --source export-file` and
`sync --type daily_activity` in `export_file` mode) never reads or writes
the keyring, and `daily` never does in any mode.

## Testing

```sh
gofmt -l .
go vet ./...
go test ./...
```

The tests build synthetic configurations, export folders, databases and cloud
responses in temporary directories and on local test servers. They never read
this machine's real configuration, database or keyring, and they never contact
the real Zepp API.
