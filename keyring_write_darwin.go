//go:build darwin

package main

import (
	"fmt"
	"io"
	"os/exec"
)

// platformWriteSecret stores a raw, Python-compatible value in the macOS
// Keychain. go-keyring's Set would prefix the value with "go-keyring-base64:",
// which the Python keyring cannot read, so the value is written the same way
// zalando does it: the command line is piped into `security -i` over stdin and
// never appears in argv, logs or output.
func platformWriteSecret(service, account, value string) error {
	cmd := exec.Command("/usr/bin/security", "-i")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("cannot open the keychain helper: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot start the keychain helper: %w", err)
	}
	if _, err := io.WriteString(stdin, securityAddCommand(service, account, value)); err != nil {
		stdin.Close()
		cmd.Wait()
		return fmt.Errorf("cannot send the credential to the keychain helper: %w", err)
	}
	if err := stdin.Close(); err != nil {
		cmd.Wait()
		return fmt.Errorf("cannot finish the keychain helper input: %w", err)
	}
	if err := cmd.Wait(); err != nil {
		// The helper never echoes the command line, but redact defensively so
		// no failure path can leak the secret.
		return fmt.Errorf("the keychain rejected the credential: %v", redactSecret(err.Error(), value))
	}
	return nil
}
