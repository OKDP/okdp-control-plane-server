package gitstore

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/okdp/okdp-control-plane-server/internal/gitops"
)

func testCertPEM(t *testing.T, cn string) []byte {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestHTTPCredentialsFromSecret(t *testing.T) {
	caNew := testCertPEM(t, "new-ca")
	caOld := testCertPEM(t, "old-ca")

	creds, err := CredentialsFromSecret("https://git.example.com/org/repo.git", nil)
	require.NoError(t, err)
	assert.Nil(t, creds.Auth, "no secret: anonymous")

	creds, err = CredentialsFromSecret("https://git.example.com/org/repo.git", map[string][]byte{
		"username": []byte("bot"), "password": []byte("s3cret"), "ca.crt": caNew, "caFile": caOld,
	})
	require.NoError(t, err)
	assert.Equal(t, &githttp.BasicAuth{Username: "bot", Password: "s3cret"}, creds.Auth)
	assert.Equal(t, caNew, creds.CABundle, "ca.crt takes precedence over caFile")

	creds, err = CredentialsFromSecret("https://git.example.com/org/repo.git", map[string][]byte{
		"ca.crt": {}, "caFile": caOld,
	})
	require.NoError(t, err)
	assert.Equal(t, caOld, creds.CABundle, "empty ca.crt falls back to caFile")

	creds, err = CredentialsFromSecret("http://git.example.com/repo.git", map[string][]byte{
		"bearerToken": []byte("tok"), "username": []byte("ignored"), "password": []byte("ignored"), "caFile": caOld,
	})
	require.NoError(t, err)
	assert.Equal(t, &githttp.TokenAuth{Token: "tok"}, creds.Auth, "bearerToken takes precedence over username/password")
	assert.Equal(t, caOld, creds.CABundle)

	for name, tc := range map[string]struct {
		data map[string][]byte
		want string
	}{
		"empty bearer token":          {map[string][]byte{"bearerToken": {}}, "empty bearerToken"},
		"bearer token with CR":        {map[string][]byte{"bearerToken": []byte("tok\rX-Evil: 1")}, "newline"},
		"bearer token with LF":        {map[string][]byte{"bearerToken": []byte("tok\n")}, "newline"},
		"password, no username":       {map[string][]byte{"password": []byte("p")}, "password is set without username"},
		"empty username, no password": {map[string][]byte{"username": {}}, "empty username"},
		"invalid ca.crt PEM":          {map[string][]byte{"ca.crt": []byte("not-a-cert")}, "valid PEM"},
		"invalid caFile PEM":          {map[string][]byte{"caFile": []byte("not-a-cert")}, "valid PEM"},
		"ssh identity over https":     {map[string][]byte{"identity": []byte("key")}, "SSH identity"},
	} {
		_, err := CredentialsFromSecret("https://git.example.com/repo.git", tc.data)
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), tc.want, name)
	}

	_, err = CredentialsFromSecret("ftp://git.example.com/repo.git", nil)
	assert.Error(t, err)
	_, err = CredentialsFromSecret("/srv/repo.git", map[string][]byte{"username": []byte("x")})
	assert.Error(t, err, "a local remote takes no credentials")
}

func TestSSHCredentialsFromSecret(t *testing.T) {
	_, hostKey := newHostKey(t)
	identity, _ := newIdentity(t, "")
	encrypted, _ := newIdentity(t, "passphrase")

	knownHosts := []byte(knownhosts.Line([]string{"git.example.com"}, hostKey) + "\n" +
		knownhosts.Line([]string{"[git.example.com]:2222"}, hostKey) + "\n")

	for _, url := range []string{
		"ssh://git@git.example.com/org/repo.git",
		"git@git.example.com:org/repo.git",
		"ssh://deploy@git.example.com:2222/org/repo.git",
	} {
		creds, err := CredentialsFromSecret(url, map[string][]byte{"identity": identity, "known_hosts": knownHosts})
		require.NoError(t, err, url)
		auth := creds.Auth.(*sshAuth)
		cfg, err := auth.ClientConfig()
		require.NoError(t, err)
		assert.Equal(t, []string{ssh.KeyAlgoED25519}, cfg.HostKeyAlgorithms, "only the algorithms known_hosts holds")
		if strings.Contains(url, "deploy@") {
			assert.Equal(t, "deploy", auth.User)
		} else {
			assert.Equal(t, "git", auth.User)
		}
	}

	creds, err := CredentialsFromSecret("ssh://git.example.com/repo.git", map[string][]byte{
		"identity": encrypted, "password": []byte("passphrase"), "known_hosts": knownHosts,
	})
	require.NoError(t, err)
	assert.Equal(t, "git", creds.Auth.(*sshAuth).User, "user from the URL, git by default")

	for name, data := range map[string]map[string][]byte{
		"no identity":          {"known_hosts": knownHosts},
		"no known_hosts":       {"identity": identity},
		"wrong passphrase":     {"identity": encrypted, "password": []byte("nope"), "known_hosts": knownHosts},
		"garbage identity":     {"identity": []byte("not a key"), "known_hosts": knownHosts},
		"garbage known_hosts":  {"identity": identity, "known_hosts": []byte("@@@ not known hosts")},
		"host not known_hosts": {"identity": identity, "known_hosts": []byte(knownhosts.Line([]string{"other.example.com"}, hostKey) + "\n")},
	} {
		_, err := CredentialsFromSecret("ssh://git@git.example.com/repo.git", data)
		assert.Error(t, err, name)
	}
}

func TestHTTPBasicAndBearerEndToEnd(t *testing.T) {
	gitBin := requireGit(t)
	bare := newBare(t)
	runGit(t, bare, "config", "http.receivepack", "true")

	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Dir:  filepath.Dir(bare),
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"},
	}
	var (
		mu   sync.Mutex
		seen []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, got)
		mu.Unlock()
		user, pass, basic := r.BasicAuth()
		basicOK := basic && user == "bot" && pass == "s3cret"
		if got != "Bearer tok-123" && !basicOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	url := server.URL + "/remote.git"

	storeWith := func(data map[string][]byte) *Store {
		creds, err := CredentialsFromSecret(url, data)
		require.NoError(t, err)
		s, err := New(Config{Repository: gitops.Repository{URL: url, Path: "clusters/dev"}, Credentials: creds})
		require.NoError(t, err)
		return s
	}

	basic := storeWith(map[string][]byte{"username": []byte("bot"), "password": []byte("s3cret")})
	_, err := basic.Bootstrap(testCtx(t))
	require.NoError(t, err)
	_, err = basic.Write(testCtx(t), oneFile("a.yaml", "basic\n"))
	require.NoError(t, err)

	bearer := storeWith(map[string][]byte{"bearerToken": []byte("tok-123"), "username": []byte("bot"), "password": []byte("wrong")})
	_, err = bearer.Write(testCtx(t), oneFile("b.yaml", "bearer\n"))
	require.NoError(t, err)
	mu.Lock()
	assert.Contains(t, seen, "Bearer tok-123")
	mu.Unlock()

	assert.Equal(t, "basic", showFile(t, bare, "clusters/dev/a.yaml"))
	assert.Equal(t, "bearer", showFile(t, bare, "clusters/dev/b.yaml"))

	wrong := storeWith(map[string][]byte{"username": []byte("bot"), "password": []byte("wrong")})
	_, err = wrong.Write(testCtx(t), oneFile("c.yaml", "x\n"))
	assert.ErrorIs(t, err, ErrUnavailable)
	anonymous := storeWith(nil)
	_, err = anonymous.Latest(testCtx(t))
	assert.ErrorIs(t, err, ErrUnavailable)
}

func TestAForbiddenPushIsRejectedWithoutRetry(t *testing.T) {
	gitBin := requireGit(t)
	bare := newBare(t)
	runGit(t, bare, "config", "http.receivepack", "true")
	pushDirect(t, bare, "main", map[string][]byte{"clusters/dev/.gitkeep": {}}, "init")
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"},
	}
	var receivePacks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "git-receive-pack") || strings.HasSuffix(r.URL.Path, "git-receive-pack") {
			receivePacks.Add(1)
			http.Error(w, "read-only token", http.StatusForbidden)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	s, err := New(Config{Repository: gitops.Repository{URL: server.URL + "/remote.git", Path: "clusters/dev"}})
	require.NoError(t, err)
	_, err = s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
	assert.ErrorIs(t, err, ErrRejected)
	assert.NotErrorIs(t, err, ErrUncertain)
	assert.Equal(t, int32(1), receivePacks.Load(), "fetching worked, the push is refused once and not retried")
}

func TestSSHEndToEndChecksTheHostKey(t *testing.T) {
	bare := newBare(t)
	hostSigner, hostKey := newHostKey(t)
	identity, clientKey := newIdentity(t, "")
	addr := serveGitOverSSH(t, hostSigner, clientKey, filepath.Dir(bare))
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	url := fmt.Sprintf("ssh://git@%s:%s/remote.git", host, port)
	hostPattern := fmt.Sprintf("[%s]:%s", host, port)

	storeWith := func(knownHosts string) (*Store, error) {
		creds, err := CredentialsFromSecret(url, map[string][]byte{"identity": identity, "known_hosts": []byte(knownHosts)})
		if err != nil {
			return nil, err
		}
		return New(Config{Repository: gitops.Repository{URL: url, Path: "clusters/dev"}, Credentials: creds})
	}

	good, err := storeWith(knownhosts.Line([]string{hostPattern}, hostKey) + "\n")
	require.NoError(t, err)
	_, err = good.Bootstrap(testCtx(t))
	require.NoError(t, err)
	_, err = good.Write(testCtx(t), oneFile("a.yaml", "over ssh\n"))
	require.NoError(t, err)
	assert.Equal(t, "over ssh", showFile(t, bare, "clusters/dev/a.yaml"))

	_, impostor := newHostKey(t)
	bad, err := storeWith(knownhosts.Line([]string{hostPattern}, impostor) + "\n")
	require.NoError(t, err)
	_, err = bad.Write(testCtx(t), oneFile("b.yaml", "x\n"))
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "key mismatch")
	assert.Equal(t, "2", commitCount(t, bare))
}

func newHostKey(t *testing.T) (ssh.Signer, ssh.PublicKey) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return signer, signer.PublicKey()
}

// newIdentity returns a PEM private key, encrypted when passphrase is set.
func newIdentity(t *testing.T, passphrase string) ([]byte, ssh.PublicKey) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	var block *pem.Block
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	require.NoError(t, err)
	sshPub, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	return pem.EncodeToMemory(block), sshPub
}

// serveGitOverSSH runs an SSH server accepting clientKey only, which runs
// git-upload-pack and git-receive-pack on repositories under root.
func serveGitOverSSH(t *testing.T, host ssh.Signer, clientKey ssh.PublicKey, root string) string {
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientKey.Marshal()) {
				return nil, fmt.Errorf("unknown client key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(host)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	env := gitEnv(t)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, requests, err := nc.Accept()
					if err != nil {
						continue
					}
					go serveGitChannel(ch, requests, root, env)
				}
			}()
		}
	}()
	return ln.Addr().String()
}

func serveGitChannel(ch ssh.Channel, requests <-chan *ssh.Request, root string, env []string) {
	defer func() { _ = ch.Close() }()
	for req := range requests {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		service, arg, _ := strings.Cut(payload.Command, " ")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			_ = req.Reply(false, nil)
			return
		}
		repo := filepath.Join(root, filepath.Base(strings.Trim(arg, "'\"")))
		_ = req.Reply(true, nil)

		cmd := exec.Command("git", strings.TrimPrefix(service, "git-"), repo)
		cmd.Env = env
		cmd.Stdout = ch
		cmd.Stderr = ch.Stderr()
		stdin, _ := cmd.StdinPipe()
		status := uint32(0)
		if err := cmd.Start(); err != nil {
			status = 1
		} else {
			go func() { _, _ = io.Copy(stdin, ch); _ = stdin.Close() }()
			if err := cmd.Wait(); err != nil {
				status = 1
			}
		}
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
		return
	}
}

func TestCredentialsInTheURLAreRefusedWithoutQuotingThem(t *testing.T) {
	for _, url := range []string{
		"https://bot:s3cret@git.example.com/repo.git",
		"http://bot:s3cret@git.example.com/repo.git",
		"https://s3cret@git.example.com/repo.git",
		"https://bot:p/s3cret@git.example.com/repo.git",
		"https://bot:123/s3cret@git.example.com/repo.git",
		"ssh://git:s3cret@git.example.com/repo.git",
		"git:s3cret@git.example.com:org/repo.git",
		"https://bot:s3cret@git example.com/%zz",
		"https://git.example.com/repo.git?token=s3cret",
		"ssh://git@git.example.com/repo.git?token=s3cret",
	} {
		_, err := New(Config{Repository: gitops.Repository{URL: url}})
		require.Error(t, err, url)
		assert.NotContains(t, err.Error(), "s3cret", url)
		assert.NotContains(t, err.Error(), "git.example.com", url)
		_, err = CredentialsFromSecret(url, nil)
		require.Error(t, err, url)
		assert.NotContains(t, err.Error(), "s3cret", url)
	}
	for _, url := range []string{"ssh://git:@git.example.com/repo.git", "https://:@git.example.com/repo.git", "https://bot@git.example.com/repo.git"} {
		_, err := New(Config{Repository: gitops.Repository{URL: url}})
		assert.Error(t, err, "an empty password or a bare user is still refused: %s", url)
	}
	for _, url := range []string{"ssh://git@git.example.com/repo.git", "git@git.example.com:org/repo.git", "https://git.example.com/repo.git"} {
		_, err := New(Config{Repository: gitops.Repository{URL: url}})
		assert.NoError(t, err, url)
	}
}

func TestSameOriginRedirect(t *testing.T) {
	req := func(raw string) *http.Request {
		r, err := http.NewRequest(http.MethodGet, raw, nil)
		require.NoError(t, err)
		return r
	}
	first := []*http.Request{req("https://git.example.com/repo.git/info/refs")}
	assert.NoError(t, sameOriginRedirect(req("https://GIT.example.com:443/other.git/info/refs"), first))
	for _, target := range []string{
		"http://git.example.com/repo.git/info/refs",
		"https://evil.example.com/repo.git/info/refs",
		"https://git.example.com:8443/repo.git/info/refs",
	} {
		assert.ErrorIs(t, sameOriginRedirect(req(target), first), errCrossOriginRedirect, target)
	}
}

func TestACrossOriginRedirectNeverReachesTheOtherServer(t *testing.T) {
	var reached atomic.Int32
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		leaked.Store(r.Header.Get("Authorization"))
		http.Error(w, "nothing here", http.StatusNotFound)
	}))
	t.Cleanup(other.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(origin.Close)

	url := origin.URL + "/remote.git"
	creds, err := CredentialsFromSecret(url, map[string][]byte{"bearerToken": []byte("tok-123")})
	require.NoError(t, err)
	s, err := New(Config{Repository: gitops.Repository{URL: url, Path: "clusters/dev"}, Credentials: creds})
	require.NoError(t, err)

	_, err = s.Latest(testCtx(t))
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "another origin")
	assert.Zero(t, reached.Load(), "the redirect was not followed")
	assert.Nil(t, leaked.Load())
}

func TestASameOriginRedirectIsFollowed(t *testing.T) {
	gitBin := requireGit(t)
	bare := newBare(t)
	runGit(t, bare, "config", "http.receivepack", "true")
	backend := &cgi.Handler{
		Path: gitBin,
		Args: []string{"http-backend"},
		Env:  []string{"GIT_PROJECT_ROOT=" + filepath.Dir(bare), "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if moved, ok := strings.CutPrefix(r.URL.Path, "/old.git/"); ok {
			http.Redirect(w, r, "/remote.git/"+moved+"?"+r.URL.RawQuery, http.StatusMovedPermanently)
			return
		}
		backend.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	s, err := New(Config{Repository: gitops.Repository{URL: server.URL + "/old.git", Path: "clusters/dev"}})
	require.NoError(t, err)
	_, err = s.Bootstrap(testCtx(t))
	require.NoError(t, err)
	assert.Equal(t, "clusters/dev/.gitkeep", runGit(t, bare, "ls-tree", "-r", "--name-only", "main"))
}

func TestARedirectLocationNeverLeaksItsCredentials(t *testing.T) {
	other := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(other.Close)
	host := strings.TrimPrefix(other.URL, "http://")
	for name, location := range map[string]string{
		"credentials in the target": "http://bot:s3cret@" + host + "/remote.git/info/refs?service=git-upload-pack",
		"malformed target":          "http://bot:s3cret@bad host/x",
		"scheme-relative target":    "//bot:s3cret@" + host + "/remote.git/info/refs?service=git-upload-pack",
		"quote in the password":     "http://bot:s3'cret@" + host + "/remote.git/info/refs?service=git-upload-pack",
	} {
		t.Run(name, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", location)
				w.WriteHeader(http.StatusFound)
			}))
			t.Cleanup(origin.Close)
			s, err := New(Config{Repository: gitops.Repository{URL: origin.URL + "/remote.git"}})
			require.NoError(t, err)
			_, err = s.Latest(testCtx(t))
			require.ErrorIs(t, err, ErrUnavailable)
			assert.NotContains(t, err.Error(), "s3cret")
			assert.NotContains(t, err.Error(), "s3'cret")
			_, err = s.Write(testCtx(t), oneFile("a.yaml", "a\n"))
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "s3cret")
			assert.NotContains(t, err.Error(), "s3'cret")
		})
	}
}

func TestACrossOriginRedirectIsRefusedWithACABundle(t *testing.T) {
	var reached atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		http.NotFound(w, r)
	}))
	t.Cleanup(other.Close)
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(origin.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: origin.Certificate().Raw})

	url := origin.URL + "/remote.git"
	creds, err := CredentialsFromSecret(url, map[string][]byte{"bearerToken": []byte("tok-123"), "ca.crt": ca})
	require.NoError(t, err)
	s, err := New(Config{Repository: gitops.Repository{URL: url}, Credentials: creds})
	require.NoError(t, err)
	_, err = s.Latest(testCtx(t))
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.Contains(t, err.Error(), "another origin", "the TLS handshake with the CA bundle succeeded, the redirect was refused")
	assert.Zero(t, reached.Load())
}

func TestScrubErrorPreservesContextSentinelsAndHidesComplexPasswords(t *testing.T) {
	for name, raw := range map[string]string{
		"at sign in password":      `Get "https://bot:p@ss@git.example.com/repo.git": EOF`,
		"space in password":        `Get "https://bot:p ass@git.example.com/repo.git": EOF`,
		"slash in password":        `Get "https://bot:p/ass@git.example.com/repo.git": EOF`,
		"question in password":     `Get "https://bot:p?ass@git.example.com/repo.git": EOF`,
		"hash in password":         `Get "https://bot:p#ass@git.example.com/repo.git": EOF`,
		"escaped quote in pass":    `Get "https://bot:p\"ass@git.example.com/repo.git": EOF`,
		"unescaped quote in pass":  `Get "https://bot:p"ass@git.example.com/repo.git": EOF`,
		"protocol relative":        `Get "//bot:p/a?b#c@git.example.com/repo.git": EOF`,
		"unquoted complex pass":    `fetch main from https://bot:p@ss/1?2#3 4@git.example.com/repo.git: failed`,
		"query token param":        `Get "https://git.example.com/repo.git?access_token=tok_secret_1&ref=main": 403`,
		"semicolon password param": `Get "https://git.example.com/repo.git?a=1;password=semi_secret": 403`,
		"scp credentials":          `ssh error on git:scp_secret@git.example.com:org/repo.git: denied`,
	} {
		t.Run(name, func(t *testing.T) {
			uerr := &url.Error{Op: "Get", URL: raw, Err: context.Canceled}
			scrubbed := scrubError(uerr)
			assert.ErrorIs(t, scrubbed, context.Canceled)
			var unwrappedURL *url.Error
			assert.False(t, errors.As(scrubbed, &unwrappedURL), "*url.Error must never stay in the unwrap chain")
			for _, secret := range []string{"bot:", "p@ss", "p ass", "p/ass", "p?ass", "p#ass", `p\"ass`, `p"ass`, "p/a?b#c", "tok_secret_1", "semi_secret", "scp_secret"} {
				assert.NotContains(t, scrubbed.Error(), secret)
			}
		})
	}

	deadlineErr := scrubError(&url.Error{Op: "Get", URL: "https://bot:secret@host/repo", Err: context.DeadlineExceeded})
	assert.ErrorIs(t, deadlineErr, context.DeadlineExceeded)
	assert.NotContains(t, deadlineErr.Error(), "secret")
	var ue *url.Error
	assert.False(t, errors.As(deadlineErr, &ue))

	// Active HTTP bearer token / basic password echoed outside a URL must also be redacted.
	tokenErr := scrubErrorWithCreds(errors.New("remote rejected bearer ghp_superSecretToken123"), Credentials{
		Auth: &githttp.TokenAuth{Token: "ghp_superSecretToken123"},
	})
	assert.NotContains(t, tokenErr.Error(), "ghp_superSecretToken123")
	assert.Contains(t, tokenErr.Error(), "***")

	basicErr := scrubErrorWithCreds(errors.New("remote echoed basic pass MyP@ss/Word!"), Credentials{
		Auth: &githttp.BasicAuth{Username: "bot", Password: "MyP@ss/Word!"},
	})
	assert.NotContains(t, basicErr.Error(), "MyP@ss/Word!")
	assert.Contains(t, basicErr.Error(), "***")
}

func TestStoreLatestPreservesContextCanceled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	s, err := New(Config{Repository: gitops.Repository{URL: server.URL + "/remote.git"}})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = s.Latest(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.ErrorIs(t, err, context.Canceled)
	var ue *url.Error
	assert.False(t, errors.As(err, &ue))
}

func TestStalledSSHHandshakeTimesOutAndIgnoresSSHConfig(t *testing.T) {
	assert.Nil(t, gitssh.DefaultSSHConfig, "host ~/.ssh/config must be disabled")

	// TCP server that accepts connections and never sends an SSH banner.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { _ = c.Close() })
		}
	}()

	_, hostKey := newHostKey(t)
	identity, _ := newIdentity(t, "")
	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	repoURL := fmt.Sprintf("ssh://git@%s:%s/remote.git", host, port)
	hostPattern := fmt.Sprintf("[%s]:%s", host, port)
	creds, err := CredentialsFromSecret(repoURL, map[string][]byte{
		"identity":    identity,
		"known_hosts": []byte(knownhosts.Line([]string{hostPattern}, hostKey) + "\n"),
	})
	require.NoError(t, err)

	s, err := New(Config{
		Repository:     gitops.Repository{URL: repoURL},
		Credentials:    creds,
		NetworkTimeout: 250 * time.Millisecond,
	})
	require.NoError(t, err)

	started := time.Now()
	_, err = s.Latest(context.Background())
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 3*time.Second, "stalled SSH handshake must be aborted by NetworkTimeout")
}

func TestStalledSSHAfterHandshakeAbortsCleanlyOnTimeout(t *testing.T) {
	hostSigner, hostKey := newHostKey(t)
	identity, clientKey := newIdentity(t, "")

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) != string(clientKey.Marshal()) {
				return nil, fmt.Errorf("unknown client key")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	// Accept SSH handshake and exec request, then stall forever without
	// writing git-upload-pack refs.
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					ch, requests, err := nc.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer func() { _ = ch.Close() }()
						for req := range requests {
							if req.Type == "exec" {
								_ = req.Reply(true, nil)
								<-done
								return
							}
							_ = req.Reply(false, nil)
						}
					}()
				}
			}()
		}
	}()

	host, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	repoURL := fmt.Sprintf("ssh://git@%s:%s/remote.git", host, port)
	hostPattern := fmt.Sprintf("[%s]:%s", host, port)
	creds, err := CredentialsFromSecret(repoURL, map[string][]byte{
		"identity":    identity,
		"known_hosts": []byte(knownhosts.Line([]string{hostPattern}, hostKey) + "\n"),
	})
	require.NoError(t, err)

	s, err := New(Config{
		Repository:     gitops.Repository{URL: repoURL},
		Credentials:    creds,
		NetworkTimeout: 250 * time.Millisecond,
	})
	require.NoError(t, err)

	started := time.Now()
	_, err = s.Latest(context.Background())
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnavailable)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, elapsed, 3*time.Second, "post-handshake stalled SSH must abort quickly without blocking Store.mu")
}

func TestSSHCredentialsHandlesIPv6Host(t *testing.T) {
	_, hostKey := newHostKey(t)
	identity, _ := newIdentity(t, "")
	kh := []byte(knownhosts.Line([]string{"[::1]:2222"}, hostKey) + "\n")

	creds, err := CredentialsFromSecret("ssh://git@[::1]:2222/org/repo.git", map[string][]byte{
		"identity":    identity,
		"known_hosts": kh,
	})
	require.NoError(t, err)
	require.NotNil(t, creds.Auth)
}
