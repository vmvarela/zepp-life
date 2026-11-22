//go:build darwin || linux

package main

import (
	"errors"

	"github.com/zalando/go-keyring"
)

// platformReadSecret reads one generic password from the OS keychain. The
// Python tool stores the same (service, account) pair with keyring's macOS and
// Secret Service backends, so the credential it already wrote is read as it is.
func platformReadSecret(service, account string) (string, error) {
	secret, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", errSecretNotFound
	}
	return secret, err
}
