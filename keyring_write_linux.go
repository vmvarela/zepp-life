//go:build linux

package main

import "github.com/zalando/go-keyring"

// platformWriteSecret stores the credential with go-keyring's Secret Service
// backend, which writes the raw value the Python tool reads back.
func platformWriteSecret(service, account, value string) error {
	return keyring.Set(service, account, value)
}
