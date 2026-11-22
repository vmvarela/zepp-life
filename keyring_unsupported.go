//go:build !darwin && !linux && !windows

package main

import (
	"fmt"
	"runtime"
)

// platformReadSecret has no backend on this platform; the commands report the
// problem instead of pretending the credential is absent.
func platformReadSecret(service, account string) (string, error) {
	return "", fmt.Errorf("reading credentials is not supported on %s", runtime.GOOS)
}

// platformWriteSecret has no backend on this platform either.
func platformWriteSecret(service, account, value string) error {
	return fmt.Errorf("storing credentials is not supported on %s", runtime.GOOS)
}
