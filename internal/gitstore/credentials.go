package gitstore

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
)

// Keys of a Secret in the format of a Flux GitRepository secretRef, so that
// the server and the sync objects can share one Secret.
const (
	SecretUsername    = "username"
	SecretPassword    = "password"
	SecretBearerToken = "bearerToken"
	SecretCACert      = "ca.crt"
	SecretCAFile      = "caFile"
	SecretIdentity    = "identity"
	SecretKnownHosts  = "known_hosts"
)

const (
	defaultSSHUser = "git"
	sshDialTimeout = 30 * time.Second
)

// Credentials authenticate the store to its remote. The zero value is
// anonymous access.
type Credentials struct {
	// Auth is nil for anonymous access.
	Auth transport.AuthMethod
	// CABundle is PEM data trusted for an HTTPS remote, on top of the system
	// roots.
	CABundle []byte
}

// CredentialsFromSecret reads the data of a Kubernetes Secret laid out like a
// Flux GitRepository secretRef, for the remote repoURL. data may be nil for
// anonymous access.
//
//   - http(s) URL: bearerToken, else username and password. A bearerToken key
//     takes precedence over username/password, which are then ignored.
//     ca.crt, else caFile, adds a CA.
//   - ssh URL (ssh:// or user@host:path): identity and known_hosts are both
//     required, password decrypts the identity. The user comes from the URL,
//     git by default. Only host key algorithms present in known_hosts for
//     the host are negotiated.
func CredentialsFromSecret(repoURL string, data map[string][]byte) (Credentials, error) {
	return credentialsFromSecret(repoURL, data, "")
}

func credentialsFromSecret(repoURL string, data map[string][]byte, knownHostsPath string) (Credentials, error) {
	ep, err := parseURL(repoURL)
	if err != nil {
		return Credentials{}, err
	}
	switch ep.Protocol {
	case "http", "https":
		return httpCredentials(data)
	case "ssh":
		return sshCredentials(ep, data, knownHostsPath)
	case "file":
		if len(data) > 0 {
			return Credentials{}, errors.New("a local git remote takes no credentials")
		}
		return Credentials{}, nil
	default:
		return Credentials{}, fmt.Errorf("unsupported git URL scheme %q", ep.Protocol)
	}
}

// CredentialsFromDir reads a Secret mounted as a directory (one file per
// key, the layout of CredentialsFromSecret) for the remote repoURL. A
// missing directory is anonymous access. When the kubelet "..data" symlink is
// present, it is resolved once so every key is read atomically from the same
// versioned directory.
func CredentialsFromDir(repoURL, dir string) (Credentials, error) {
	readDir := dir
	usedDotData := false
	if target, err := os.Readlink(filepath.Join(dir, "..data")); err == nil {
		if filepath.IsAbs(target) || filepath.Clean(target) != target || target == "." || target == ".." || strings.ContainsAny(target, `/\`) {
			return Credentials{}, errors.New("invalid ..data symlink target in credentials directory")
		}
		readDir = filepath.Join(dir, target)
		fi, err := os.Lstat(readDir)
		if err != nil {
			return Credentials{}, err
		}
		if !fi.IsDir() {
			return Credentials{}, errors.New("..data target is not a directory")
		}
		usedDotData = true
	}

	entries, err := os.ReadDir(readDir)
	if errors.Is(err, os.ErrNotExist) {
		if !usedDotData {
			return CredentialsFromSecret(repoURL, nil)
		}
		return Credentials{}, err
	}
	if err != nil {
		return Credentials{}, err
	}
	data := map[string][]byte{}
	var knownHostsPath string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "..") {
			continue
		}
		full := filepath.Join(readDir, e.Name())
		info, err := os.Lstat(full)
		if err != nil {
			return Credentials{}, err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		content, err := os.ReadFile(full)
		if err != nil {
			return Credentials{}, err
		}
		data[e.Name()] = content
		if e.Name() == SecretKnownHosts {
			knownHostsPath = full
		}
	}
	return credentialsFromSecret(repoURL, data, knownHostsPath)
}

// DirCredentialsProvider returns a Config.CredentialsProvider reading dir
// on every call, so that a rotated Secret applies at the next fetch.
func DirCredentialsProvider(repoURL, dir string) func() (Credentials, error) {
	return func() (Credentials, error) { return CredentialsFromDir(repoURL, dir) }
}

func httpCredentials(data map[string][]byte) (Credentials, error) {
	if _, ok := data[SecretIdentity]; ok {
		return Credentials{}, fmt.Errorf("the secret holds an SSH %s but the git URL is HTTP", SecretIdentity)
	}
	var creds Credentials
	if ca := data[SecretCACert]; len(ca) > 0 {
		creds.CABundle = ca
	} else if ca := data[SecretCAFile]; len(ca) > 0 {
		creds.CABundle = ca
	}
	if len(creds.CABundle) > 0 {
		if !x509.NewCertPool().AppendCertsFromPEM(creds.CABundle) {
			return Credentials{}, errors.New("the CA bundle in the secret contains no valid PEM certificate")
		}
	}

	if token, ok := data[SecretBearerToken]; ok {
		if len(token) == 0 {
			return Credentials{}, fmt.Errorf("empty %s", SecretBearerToken)
		}
		if bytes.ContainsAny(token, "\r\n") {
			return Credentials{}, fmt.Errorf("%s must not contain newline characters", SecretBearerToken)
		}
		creds.Auth = &githttp.TokenAuth{Token: string(token)}
		return creds, nil
	}

	username, hasUser := data[SecretUsername]
	password, hasPassword := data[SecretPassword]
	switch {
	case !hasUser && !hasPassword:
		return creds, nil
	case len(username) == 0 && len(password) > 0:
		return Credentials{}, fmt.Errorf("%s is set without %s", SecretPassword, SecretUsername)
	case len(username) == 0:
		return Credentials{}, fmt.Errorf("empty %s", SecretUsername)
	}
	creds.Auth = &githttp.BasicAuth{Username: string(username), Password: string(password)}
	return creds, nil
}

func sshCredentials(ep *transport.Endpoint, data map[string][]byte, knownHostsPath string) (Credentials, error) {
	identity := data[SecretIdentity]
	if len(identity) == 0 {
		return Credentials{}, fmt.Errorf("an SSH git URL needs %s in the secret", SecretIdentity)
	}
	knownHosts := data[SecretKnownHosts]
	if len(knownHosts) == 0 {
		return Credentials{}, fmt.Errorf("an SSH git URL needs %s in the secret: host keys are never accepted unverified", SecretKnownHosts)
	}

	user := ep.User
	if user == "" {
		user = defaultSSHUser
	}
	keys, err := gitssh.NewPublicKeys(user, identity, string(data[SecretPassword]))
	if err != nil {
		return Credentials{}, fmt.Errorf("could not read the SSH %s: %w", SecretIdentity, err)
	}

	var db *knownhosts.HostKeyDB
	if knownHostsPath != "" {
		db, err = knownhosts.NewDB(knownHostsPath)
		if err != nil {
			return Credentials{}, fmt.Errorf("could not parse %s: %w", SecretKnownHosts, err)
		}
	} else {
		db, err = knownHostsDB(knownHosts)
		if err != nil {
			return Credentials{}, err
		}
	}
	port := ep.Port
	if port == 0 {
		port = 22
	}
	host := strings.Trim(ep.Host, "[]")
	hostWithPort := net.JoinHostPort(host, fmt.Sprint(port))
	if len(db.HostKeys(hostWithPort)) == 0 {
		return Credentials{}, fmt.Errorf("%s has no key for %s", SecretKnownHosts, knownhosts.Normalize(hostWithPort))
	}
	// Set before ClientConfig runs: with no callback, go-git looks for the
	// known_hosts files of the system and fails without any.
	keys.HostKeyCallback = db.HostKeyCallback()
	return Credentials{Auth: &sshAuth{PublicKeys: keys, algorithms: db.HostKeyAlgorithms(hostWithPort)}}, nil
}

// knownHostsDB parses in-memory known_hosts data via a temporary file. Used
// only for CredentialsFromSecret when no file path on disk is available.
func knownHostsDB(data []byte) (*knownhosts.HostKeyDB, error) {
	f, err := os.CreateTemp("", "known_hosts-*")
	if err != nil {
		return nil, fmt.Errorf("could not stage %s: %w", SecretKnownHosts, err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, werr := f.Write(data)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		return nil, fmt.Errorf("could not stage %s: %w", SecretKnownHosts, err)
	}
	db, err := knownhosts.NewDB(f.Name())
	if err != nil {
		return nil, fmt.Errorf("could not parse %s: %w", SecretKnownHosts, err)
	}
	return db, nil
}

// sshAuth is public key authentication checking the host against the
// secret's known_hosts. go-git only restricts host key algorithms for its
// own default callback, so it is done here.
type sshAuth struct {
	*gitssh.PublicKeys
	algorithms []string
}

func (a *sshAuth) ClientConfig() (*ssh.ClientConfig, error) {
	cfg, err := a.PublicKeys.ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.HostKeyAlgorithms = a.algorithms
	cfg.Timeout = sshDialTimeout
	return cfg, nil
}

// parseURL parses a git URL and refuses credentials or query strings embedded
// in it: they come from the secret only, and go-git repeats the URL, password
// included, in its endpoint and error strings. The errors never quote the URL.
func parseURL(raw string) (*transport.Endpoint, error) {
	if strings.Contains(raw, "?") {
		return nil, errors.New("the git URL must not contain a query string")
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			if strings.Contains(raw, "@") {
				return nil, errors.New("the git URL holds credentials: credentials come from the secret only")
			}
			return nil, errors.New("invalid git URL")
		}
		if u.RawQuery != "" {
			return nil, errors.New("the git URL must not contain a query string")
		}
		scheme := strings.ToLower(u.Scheme)
		if u.User != nil {
			if _, set := u.User.Password(); set {
				return nil, errors.New("the git URL holds a password: credentials come from the secret only")
			}
			if scheme == "http" || scheme == "https" {
				return nil, errors.New("the git URL holds a user: HTTP credentials come from the secret only")
			}
		}
		if (scheme == "http" || scheme == "https") && strings.Contains(raw, "@") {
			return nil, errors.New("the git URL holds credentials: HTTP credentials come from the secret only")
		}
		if strings.Contains(u.Host, ":") && !strings.HasPrefix(u.Host, "[") &&
			(strings.Contains(u.EscapedPath(), "@") || strings.Contains(u.EscapedFragment(), "@")) {
			return nil, errors.New("the git URL holds a password: credentials come from the secret only")
		}
	} else if !strings.HasPrefix(raw, "/") && !strings.HasPrefix(raw, ".") {
		if firstAt := strings.IndexByte(raw, '@'); firstAt >= 0 && strings.Contains(raw[:firstAt], ":") {
			return nil, errors.New("the git URL holds a password: credentials come from the secret only")
		}
	}
	ep, err := transport.NewEndpoint(raw)
	if err != nil {
		// The parse error quotes the URL.
		return nil, errors.New("invalid git URL")
	}
	httpURL := ep.Protocol == "http" || ep.Protocol == "https"
	hasPassword := ep.Password != ""
	hasUser := ep.User != ""
	switch {
	case hasPassword:
		return nil, errors.New("the git URL holds a password: credentials come from the secret only")
	case hasUser && httpURL:
		return nil, errors.New("the git URL holds a user: HTTP credentials come from the secret only")
	}
	return ep, nil
}

// scrubbedError carries a credential-free message while preserving only
// context cancellation/deadline sentinels (never *url.Error).
type scrubbedError struct {
	msg      string
	sentinel error
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.sentinel }

// scrubError turns a transport error into text without the credentials a
// URL in it may carry, such as the Location of a refused redirect. The raw
// error chain (which may contain *url.Error) is dropped; only
// context.Canceled and context.DeadlineExceeded are preserved for callers.
func scrubError(err error) error {
	return scrubErrorWithCreds(err, Credentials{})
}

// scrubErrorWithCreds behaves like scrubError and also redacts any active
// HTTP bearer token or basic-auth password from creds if a remote or transport
// echoed it back in the error string.
func scrubErrorWithCreds(err error, creds Credentials) error {
	if err == nil {
		return nil
	}
	var sentinel error
	switch {
	case errors.Is(err, context.Canceled):
		sentinel = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		sentinel = context.DeadlineExceeded
	}
	msg := scrubCredentials(err.Error())
	msg = redactActiveCredentials(msg, creds)
	return &scrubbedError{
		msg:      msg,
		sentinel: sentinel,
	}
}

func redactActiveCredentials(msg string, creds Credentials) string {
	redact := func(secret string) {
		if secret == "" {
			return
		}
		msg = strings.ReplaceAll(msg, secret, "***")
		if escaped := url.QueryEscape(secret); escaped != "" && escaped != secret {
			msg = strings.ReplaceAll(msg, escaped, "***")
		}
	}
	switch a := creds.Auth.(type) {
	case *githttp.TokenAuth:
		if a != nil {
			redact(a.Token)
		}
	case *githttp.BasicAuth:
		if a != nil {
			redact(a.Password)
		}
	}
	return msg
}

var (
	scrubLineUserInfoRe = regexp.MustCompile(`//[^\s\r\n]*@`)
	scrubQuerySecretRe  = regexp.MustCompile(`(?i)([?&;](?:password|passwd|pwd|token|private_token|access_token|secret|api_key|private-token|access-token|api-key|client_secret|secret_key|access_key)=)[^&\s"'#;]+`)
	scrubSCPUserInfoRe  = regexp.MustCompile(`[^\s/:"']+:[^\s/@]+@`)
)

// scrubCredentials masks URL userinfo in s, even when passwords contain '@',
// spaces, '/', '?', '#', quotes, or backslashes, as well as sensitive query
// parameters and SCP-style credentials. Double-quoted tokens (as emitted by
// *url.Error) are scrubbed independently first so surrounding text stays
// readable, followed by a full-line sweep that cannot be broken by unescaped
// quotes.
func scrubCredentials(s string) string {
	var out strings.Builder
	out.Grow(len(s))

	i := 0
	for i < len(s) {
		if s[i] == '"' {
			// Scan for matching unescaped closing quote on the same line.
			j := i + 1
			closed := false
			for j < len(s) && s[j] != '\n' && s[j] != '\r' {
				if s[j] == '\\' && j+1 < len(s) {
					j += 2
					continue
				}
				if s[j] == '"' {
					closed = true
					break
				}
				j++
			}
			if closed {
				out.WriteByte('"')
				out.WriteString(scrubSegment(s[i+1 : j]))
				out.WriteByte('"')
				i = j + 1
				continue
			}
		}

		// Accumulate unquoted text up to the next line break or next quoted string.
		start := i
		for i < len(s) && s[i] != '\n' && s[i] != '\r' {
			if s[i] == '"' && hasClosingQuoteOnLine(s, i) {
				break
			}
			i++
		}
		out.WriteString(scrubSegment(s[start:i]))
		if i < len(s) && (s[i] == '\n' || s[i] == '\r') {
			out.WriteByte(s[i])
			i++
		}
	}
	res := out.String()
	res = scrubLineUserInfoRe.ReplaceAllString(res, "//***@")
	res = scrubQuerySecretRe.ReplaceAllString(res, "${1}***")
	res = scrubSCPUserInfoRe.ReplaceAllString(res, "***@")
	return res
}

func hasClosingQuoteOnLine(s string, openIdx int) bool {
	for j := openIdx + 1; j < len(s) && s[j] != '\n' && s[j] != '\r'; j++ {
		if s[j] == '\\' && j+1 < len(s) {
			j++
			continue
		}
		if s[j] == '"' {
			return true
		}
	}
	return false
}

// scrubSegment masks from the first "//" to the last "@" within a single
// quoted string or unquoted line segment.
func scrubSegment(seg string) string {
	idx := strings.Index(seg, "//")
	if idx < 0 {
		return seg
	}
	after := seg[idx+2:]
	lastAt := strings.LastIndexByte(after, '@')
	if lastAt < 0 {
		return seg
	}
	return seg[:idx+2] + "***" + after[lastAt:]
}
