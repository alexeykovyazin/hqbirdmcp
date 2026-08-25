// Package secrets implements the D5 keyring provider (phase8_plan D4.3 /
// improvement-plan E.4, per ADR-029-adjacent ADR-009 rollout): OS credential
// store as a fallback when an environment variable is unset. Env always
// wins — "env remains supported" is the compat contract; the keyring is
// keyed by the env-var NAME (e.g. fbmcpctl secret set FBMCP_DEV_PW), so a
// config that names FBMCP_DEV_PW works unchanged with either source.
package secrets

import (
	"fmt"
	"os"

	keyring "github.com/zalando/go-keyring"
)

const service = "fbmcp"

// Get returns the secret for envName: the environment variable if set,
// otherwise the OS keyring entry "fbmcp/<envName>".
func Get(envName string) (string, error) {
	if envName == "" {
		return "", fmt.Errorf("empty secret env name")
	}
	if v := os.Getenv(envName); v != "" {
		return v, nil
	}
	v, err := keyring.Get(service, envName)
	if err == nil && v != "" {
		return v, nil
	}
	return "", fmt.Errorf("secret env %s is not set (and no keyring entry fbmcp/%s)", envName, envName)
}

// Set writes the keyring entry (fbmcpctl secret set).
func Set(envName, value string) error {
	if envName == "" || value == "" {
		return fmt.Errorf("secret name and value are required")
	}
	return keyring.Set(service, envName, value)
}

// Drop removes the keyring entry (fbmcpctl secret drop).
func Drop(envName string) error {
	return keyring.Delete(service, envName)
}
