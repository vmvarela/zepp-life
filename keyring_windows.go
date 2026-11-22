//go:build windows

package main

import (
	"errors"
	"fmt"

	"github.com/danieljoos/wincred"
)

// platformReadSecret reads the credential the Python tool wrote through
// keyring's Windows backend. That backend keeps the first account under the
// plain service name and moves earlier accounts to "<account>@<service>", so
// this lookup mirrors keyring.backends.windows.WinVaultKeyring._resolve_credential.
// The credential is read as it is: no new credential is created and the user is
// never asked to authenticate again.
func platformReadSecret(service, account string) (string, error) {
	cred, err := readTarget(service)
	if err != nil && !errors.Is(err, errSecretNotFound) {
		return "", err
	}
	if err == nil && (account == "" || cred.UserName == account) {
		return decodeCredentialBlob(cred.CredentialBlob), nil
	}
	cred, err = readTarget(account + "@" + service)
	if err != nil {
		return "", err
	}
	return decodeCredentialBlob(cred.CredentialBlob), nil
}

func readTarget(target string) (*wincred.Credential, error) {
	got, err := wincred.GetGenericCredential(target)
	if errors.Is(err, wincred.ErrElementNotFound) {
		return nil, errSecretNotFound
	}
	if err != nil {
		return nil, err
	}
	return &got.Credential, nil
}

// platformWriteSecret stores the credential with the same target, user name
// and UTF-16LE blob layout the Python tool writes, so both directions stay
// compatible.
func platformWriteSecret(service, account, value string) error {
	var (
		existingUser string
		existingBlob []byte
		hasExisting  bool
	)
	if cred, err := readTarget(service); err == nil {
		existingUser, existingBlob, hasExisting = cred.UserName, cred.CredentialBlob, true
	} else if !errors.Is(err, errSecretNotFound) {
		return err
	}
	for _, write := range planWindowsWrite(service, account, value, existingUser, existingBlob, hasExisting) {
		stored := wincred.NewGenericCredential(write.target)
		stored.UserName = write.username
		stored.CredentialBlob = write.blob
		stored.Persist = wincred.PersistEnterprise // the value Python's keyring uses
		if err := stored.Write(); err != nil {
			return fmt.Errorf("cannot store credential for service %q: %w", service, err)
		}
	}
	return nil
}
