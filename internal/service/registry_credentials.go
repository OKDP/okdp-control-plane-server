package service

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type registryCredential struct {
	username string
	password string
}

// registryCredentialFor returns the credential a dockerconfigjson file holds
// for a registry host, or nil when it has none. The file is read on every call
// so a rotated secret is picked up without restarting the server.
func registryCredentialFor(authFile, host string) (*registryCredential, error) {
	data, err := os.ReadFile(authFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read registry credentials: %w", err)
	}

	var config struct {
		Auths map[string]struct {
			Auth     string `json:"auth"`
			Username string `json:"username"`
			Password string `json:"password"`
		} `json:"auths"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse registry credentials %s: %w", authFile, err)
	}

	for key, entry := range config.Auths {
		if registryHostOf(key) != host {
			continue
		}
		if entry.Username != "" && entry.Password != "" {
			return &registryCredential{username: entry.Username, password: entry.Password}, nil
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
		if err != nil {
			return nil, fmt.Errorf("invalid auth entry for %s: %w", host, err)
		}
		username, password, found := strings.Cut(string(decoded), ":")
		if !found {
			return nil, fmt.Errorf("invalid auth entry for %s: expected username:password", host)
		}
		return &registryCredential{username: username, password: password}, nil
	}
	return nil, nil
}

// registryHostOf reduces a dockerconfigjson key, sometimes written as a URL
// ("https://ghcr.io/v1/"), to its host.
func registryHostOf(key string) string {
	key = strings.TrimPrefix(key, "https://")
	key = strings.TrimPrefix(key, "http://")
	host, _, _ := strings.Cut(key, "/")
	return host
}

// kubocdCredentialEnv passes a credential to `kubocd dump` through the
// per-registry variables it reads (KCD_OCI_<HOST>_USER / _SECRET).
func kubocdCredentialEnv(host string, cred *registryCredential) []string {
	tag := strings.ToUpper(strings.NewReplacer(":", "_", "/", "_", "-", "_", ".", "_").Replace(host))
	return []string{
		fmt.Sprintf("KCD_OCI_%s_USER=%s", tag, cred.username),
		fmt.Sprintf("KCD_OCI_%s_SECRET=%s", tag, cred.password),
	}
}
