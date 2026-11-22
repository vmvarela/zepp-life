package main

import "errors"

// Names used by the Python tool: service "zepp-life-mcp" with the accounts
// "zepp_auth_token" and "zepp_auth_user_id".
const (
	keyringService       = "zepp-life-mcp"
	keyringTokenAccount  = "zepp_auth_token"
	keyringUserIDAccount = "zepp_auth_user_id"
)

// errSecretNotFound reports an absent credential. It is a normal state, not a
// failure; platform readers translate their own "not found" errors into it.
var errSecretNotFound = errors.New("credential not found")

// readCredential returns a stored secret and whether it exists. An empty secret
// counts as absent, the same way the Python tool treats it.
func readCredential(env environment, account string) (value string, found bool, err error) {
	secret, err := env.readSecret(keyringService, account)
	if err != nil {
		if errors.Is(err, errSecretNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	if secret == "" {
		return "", false, nil
	}
	return secret, true, nil
}

// writeCredential stores a secret under the Python tool's service and account.
// The value is written through the platform writer; it is never logged, never
// printed and never passed on a command line as an argument.
func writeCredential(env environment, account, value string) error {
	if value == "" {
		return errors.New("refusing to store an empty credential")
	}
	return env.writeSecret(keyringService, account, value)
}
