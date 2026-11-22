package main

import (
	"strings"
	"testing"
)

func TestSecurityAddCommandQuotesTheSecretForStdin(t *testing.T) {
	const prefix = "add-generic-password -U -s zepp-life-mcp -a zepp_auth_token "
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{"plain token", "abc123", prefix + "-w abc123\n"},
		{"spaces", "a b", prefix + "-w 'a b'\n"},
		{"single quote", "it's", prefix + "-w 'it'\"'\"'s'\n"},
		{"empty", "", prefix + "-w ''\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := securityAddCommand("zepp-life-mcp", "zepp_auth_token", test.value)
			if got != test.want {
				t.Errorf("securityAddCommand() = %q, want %q", got, test.want)
			}
			if !strings.HasPrefix(got, "add-generic-password ") {
				t.Errorf("command = %q, want the add-generic-password subcommand", got)
			}
			if !strings.Contains(got, " -w ") {
				t.Errorf("command = %q, want the value after -w on stdin, never in argv", got)
			}
		})
	}
}

func TestQuoteForStdinCannotBreakOutOfTheToken(t *testing.T) {
	// Whatever the value contains, it stays one token for the `security -i`
	// parser: a quote, space or semicolon can never start a new word.
	value := `x" ; rm -rf ~ ; echo '`
	quoted := quoteForStdin(value)

	inSingle, inDouble := false, false
	for _, r := range quoted {
		switch {
		case inSingle:
			if r == '\'' {
				inSingle = false
			}
		case inDouble:
			if r == '"' {
				inDouble = false
			}
		case r == '\'':
			inSingle = true
		case r == '"':
			inDouble = true
		case r == ' ' || r == ';' || r == '\n':
			t.Fatalf("unquoted %q in %q would split the token", r, quoted)
		}
	}
	if inSingle || inDouble {
		t.Fatalf("unbalanced quoting in %q", quoted)
	}
}

func TestEncodeUTF16LEMatchesThePywin32Blob(t *testing.T) {
	blob := encodeUTF16LE("abc")
	want := []byte{0x61, 0x00, 0x62, 0x00, 0x63, 0x00}
	if len(blob) != len(want) {
		t.Fatalf("blob = %v, want %v", blob, want)
	}
	for i := range want {
		if blob[i] != want[i] {
			t.Fatalf("blob = %v, want %v (UTF-16LE, no BOM, no NUL)", blob, want)
		}
	}
	// Non-ASCII survives the round trip through the existing reader.
	secret := "t0ken-ü-ñ"
	if got := decodeCredentialBlob(encodeUTF16LE(secret)); got != secret {
		t.Errorf("round trip = %q, want %q", got, secret)
	}
}

func TestCompoundTarget(t *testing.T) {
	if got := compoundTarget("zepp_auth_token", "zepp-life-mcp"); got != "zepp_auth_token@zepp-life-mcp" {
		t.Errorf("compoundTarget() = %q", got)
	}
}

func TestPlanWindowsWriteMirrorsKeyringTargetSelection(t *testing.T) {
	existingBlob := encodeUTF16LE("old-token")

	t.Run("first credential uses the plain service target", func(t *testing.T) {
		writes := planWindowsWrite("svc", "zepp_auth_token", "token", "", nil, false)
		if len(writes) != 1 {
			t.Fatalf("writes = %+v, want exactly one", writes)
		}
		if writes[0].target != "svc" || writes[0].username != "zepp_auth_token" {
			t.Errorf("write = %+v", writes[0])
		}
		if string(writes[0].blob) != string(encodeUTF16LE("token")) {
			t.Errorf("blob = %v, want UTF-16LE of the new value", writes[0].blob)
		}
	})

	t.Run("a second account moves the first to its compound target", func(t *testing.T) {
		writes := planWindowsWrite("svc", "zepp_auth_user_id", "123456",
			"zepp_auth_token", existingBlob, true)
		if len(writes) != 2 {
			t.Fatalf("writes = %+v, want a move then the new value", writes)
		}
		moved := writes[0]
		if moved.target != "zepp_auth_token@svc" || moved.username != "zepp_auth_token" {
			t.Errorf("move = %+v, want the existing entry under its compound name", moved)
		}
		if string(moved.blob) != string(existingBlob) {
			t.Errorf("moved blob changed: %v", moved.blob)
		}
		if writes[1].target != "svc" || writes[1].username != "zepp_auth_user_id" {
			t.Errorf("new = %+v, want the plain service target", writes[1])
		}
	})

	t.Run("rewriting the same account still moves the old value aside", func(t *testing.T) {
		writes := planWindowsWrite("svc", "zepp_auth_token", "new", "zepp_auth_token", existingBlob, true)
		if len(writes) != 2 ||
			writes[0].target != "zepp_auth_token@svc" ||
			writes[1].target != "svc" || writes[1].username != "zepp_auth_token" {
			t.Fatalf("writes = %+v", writes)
		}
		if string(writes[1].blob) != string(encodeUTF16LE("new")) {
			t.Errorf("new blob = %v", writes[1].blob)
		}
	})
}

func TestRedactSecret(t *testing.T) {
	if got := redactSecret("error: token abc123 rejected", "abc123"); got != "error: token [redacted] rejected" {
		t.Errorf("redactSecret() = %q", got)
	}
	if got := redactSecret("plain error", "abc123"); got != "plain error" {
		t.Errorf("redactSecret() = %q", got)
	}
	if got := redactSecret("error", ""); got != "error" {
		t.Errorf("redactSecret() = %q", got)
	}
}
