package main

import (
	"regexp"
	"strings"
	"unicode/utf16"
)

// shellescapePattern mirrors the quoting go-keyring feeds to `security -i`:
// only word characters and a few safe punctuation marks stay unquoted, so the
// token can never be split into extra arguments or interpreted by the parser.
var shellescapePattern = regexp.MustCompile(`[^\w@%+=:,./-]`)

// quoteForStdin escapes one token for a command line that is written to the
// standard input of `security -i`. The secret is never placed in argv.
func quoteForStdin(s string) string {
	if len(s) == 0 {
		return "''"
	}
	if shellescapePattern.MatchString(s) {
		return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
	}
	return s
}

// securityAddCommand builds the `security -i` command line that stores a raw
// (Python-compatible) generic password. Callers write it to the process stdin.
func securityAddCommand(service, account, value string) string {
	return "add-generic-password -U -s " + quoteForStdin(service) +
		" -a " + quoteForStdin(account) +
		" -w " + quoteForStdin(value) + "\n"
}

// encodeUTF16LE encodes a credential blob the way pywin32 does: UTF-16 code
// units, little endian, no byte order mark and no trailing NUL.
func encodeUTF16LE(s string) []byte {
	units := utf16.Encode([]rune(s))
	blob := make([]byte, len(units)*2)
	for i, u := range units {
		blob[2*i] = byte(u)
		blob[2*i+1] = byte(u >> 8)
	}
	return blob
}

// compoundTarget is the "{username}@{service}" name keyring uses on Windows
// when several accounts share one service.
func compoundTarget(username, service string) string {
	return username + "@" + service
}

// decodeCredentialBlob decodes the UTF-16 blob pywin32 writes, tolerating a
// byte order mark and trailing NUL units. Anything that is not valid UTF-16 is
// returned unchanged, the same way keyring falls back to UTF-8.
func decodeCredentialBlob(blob []byte) string {
	if len(blob) == 0 || len(blob)%2 != 0 {
		return string(blob)
	}
	units := make([]uint16, len(blob)/2)
	for i := range units {
		units[i] = uint16(blob[2*i]) | uint16(blob[2*i+1])<<8
	}
	if units[0] == 0xFEFF {
		units = units[1:]
	}
	return strings.TrimRight(string(utf16.Decode(units)), "\x00")
}

// redactSecret removes any accidental occurrence of the secret from a message.
func redactSecret(message, secret string) string {
	if secret == "" {
		return message
	}
	return strings.ReplaceAll(message, secret, "[redacted]")
}

// credWrite is one planned Credential Manager write.
type credWrite struct {
	target   string
	username string
	blob     []byte
}

// planWindowsWrite mirrors keyring.backends.windows.WinVaultKeyring.set_password:
// whatever already sits under the plain service target is first re-saved under
// its compound name, then the new value is written under the service target
// with the requested user name. That is exactly the layout the read path in
// keyring_windows.go resolves, so credentials written here stay readable by
// the Python tool.
func planWindowsWrite(service, account, value string, existingUser string, existingBlob []byte, hasExisting bool) []credWrite {
	var writes []credWrite
	if hasExisting {
		writes = append(writes, credWrite{
			target:   compoundTarget(existingUser, service),
			username: existingUser,
			blob:     existingBlob,
		})
	}
	return append(writes, credWrite{
		target:   service,
		username: account,
		blob:     encodeUTF16LE(value),
	})
}
