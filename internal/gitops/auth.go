package gitops

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"golang.org/x/crypto/ssh"
)

// Credentials file names, the keys of the credentials Secret mounted in the
// pod. They follow the Flux GitRepository Secret convention, so the Secret the
// engine reads the repository with can be reused as is (with write access).
const (
	credUsername    = "username"
	credPassword    = "password"
	credBearerToken = "bearerToken"
	credIdentity    = "identity"
	credKnownHosts  = "known_hosts"
	credPassphrase  = "passphrase"
)

// AuthFromDir builds the transport credentials for url from a directory holding
// the credentials Secret. A missing or empty directory means anonymous access.
//
//   - HTTPS: username + password (a token as password), or bearerToken.
//   - SSH: identity (a private key), known_hosts (required unless
//     insecureIgnoreHostKey), optional passphrase.
func AuthFromDir(url, dir string, insecureIgnoreHostKey bool) (transport.AuthMethod, error) {
	read := func(name string) string {
		if dir == "" {
			return ""
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return ""
		}
		return string(data)
	}

	if isSSHURL(url) {
		identity := read(credIdentity)
		if identity == "" {
			return nil, fmt.Errorf("an SSH repository URL needs an %q key in the credentials Secret", credIdentity)
		}
		user := "git"
		if endpoint, err := transport.NewEndpoint(url); err == nil && endpoint.User != "" {
			user = endpoint.User
		}
		keys, err := gitssh.NewPublicKeys(user, []byte(identity), strings.TrimSpace(read(credPassphrase)))
		if err != nil {
			return nil, fmt.Errorf("invalid SSH identity: %w", err)
		}
		switch {
		case read(credKnownHosts) != "":
			callback, err := gitssh.NewKnownHostsCallback(filepath.Join(dir, credKnownHosts))
			if err != nil {
				return nil, fmt.Errorf("invalid known_hosts: %w", err)
			}
			keys.HostKeyCallback = callback
		case insecureIgnoreHostKey:
			keys.HostKeyCallback = ssh.InsecureIgnoreHostKey() // #nosec G106 -- explicit opt-in, sandboxes only
		default:
			return nil, errors.New("an SSH repository URL needs a known_hosts key in the credentials Secret")
		}
		return keys, nil
	}

	if token := strings.TrimSpace(read(credBearerToken)); token != "" {
		return &githttp.TokenAuth{Token: token}, nil
	}
	username := strings.TrimSpace(read(credUsername))
	password := strings.TrimSpace(read(credPassword))
	if username == "" && password == "" {
		return nil, nil
	}
	if username == "" {
		// Most forges accept any user name with a token as password.
		username = "git"
	}
	return &githttp.BasicAuth{Username: username, Password: password}, nil
}

func isSSHURL(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	if strings.Contains(url, "://") {
		return false
	}
	// scp-like syntax: git@host:org/repo.git
	at := strings.Index(url, "@")
	colon := strings.Index(url, ":")
	return at >= 0 && colon > at
}
