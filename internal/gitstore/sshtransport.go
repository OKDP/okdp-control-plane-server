package gitstore

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/pktline"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/sideband"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/go-git/go-git/v5/utils/ioutil"
	"golang.org/x/crypto/ssh"
)

// go-git's built-in SSH transport reads ~/.ssh/config by default and runs
// ssh.NewClientConn without a deadline on the TCP connection, so a stalled
// SSH handshake blocks indefinitely. We disable DefaultSSHConfig and install
// a hardened SSH transport that ignores host ssh_config files, enforces a
// deadline across both TCP dial and SSH handshake, and respects context
// cancellation.
func init() {
	gitssh.DefaultSSHConfig = nil
	client.InstallProtocol("ssh", &sshTransport{})
}

var (
	sshStdErrSkipPattern = regexp.MustCompile("^remote:( =*){0,1}$")
	sshErrTimeout        = errors.New("timeout exceeded")
)

const sshReadErrorTimeout = 10 * time.Second

type sshTransport struct{}

func (t *sshTransport) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	return newSSHSession(transport.UploadPackServiceName, ep, auth)
}

func (t *sshTransport) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	return newSSHSession(transport.ReceivePackServiceName, ep, auth)
}

type sshSession struct {
	service       string
	endpoint      *transport.Endpoint
	auth          gitssh.AuthMethod
	isReceivePack bool

	mu           sync.Mutex
	connected    bool
	closed       bool
	conn         net.Conn
	client       *ssh.Client
	session      *ssh.Session
	stdin        io.WriteCloser
	stdout       io.Reader
	firstErrLine chan string
	contextStops []func() bool

	advRefs  *packp.AdvRefs
	packRun  bool
	finished bool
}

func newSSHSession(service string, ep *transport.Endpoint, auth transport.AuthMethod) (*sshSession, error) {
	if auth == nil {
		return nil, errors.New("SSH credentials are required")
	}
	sshAuthMethod, ok := auth.(gitssh.AuthMethod)
	if !ok {
		return nil, transport.ErrInvalidAuthMethod
	}
	return &sshSession{
		service:       service,
		endpoint:      ep,
		auth:          sshAuthMethod,
		isReceivePack: service == transport.ReceivePackServiceName,
	}, nil
}

func (s *sshSession) ensureConnected(ctx context.Context) error {
	s.mu.Lock()
	if s.connected {
		s.mu.Unlock()
		return nil
	}
	if s.closed {
		s.mu.Unlock()
		return net.ErrClosed
	}
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	cfg, err := s.auth.ClientConfig()
	if err != nil {
		return err
	}
	if cfg.HostKeyCallback == nil {
		return errors.New("SSH host key callback is required")
	}

	port := s.endpoint.Port
	if port <= 0 {
		port = gitssh.DefaultPort
	}
	host := strings.Trim(s.endpoint.Host, "[]")
	addr := net.JoinHostPort(host, strconv.Itoa(port))

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = sshDialTimeout
	}
	deadline := time.Now().Add(timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	dialCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var d net.Dialer
	conn, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("SSH dial %s: %w", addr, ctx.Err())
		}
		if dialCtx.Err() != nil {
			return fmt.Errorf("SSH dial %s: %w", addr, dialCtx.Err())
		}
		return err
	}

	// Bound the SSH handshake, session open, and remote command start with
	// both an explicit socket deadline and cancellation watcher so a hung
	// peer can never block forever.
	_ = conn.SetDeadline(deadline)
	stopWatch := context.AfterFunc(dialCtx, func() {
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	})

	cleanup := func(c *ssh.Client, sess *ssh.Session) {
		stopWatch()
		_ = conn.SetDeadline(time.Now())
		if c != nil {
			_ = c.Close()
		}
		_ = conn.Close()
		if sess != nil {
			_ = sess.Close()
		}
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		cleanup(nil, nil)
		if ctx.Err() != nil {
			return fmt.Errorf("SSH handshake %s: %w", addr, ctx.Err())
		}
		if dialCtx.Err() != nil {
			return fmt.Errorf("SSH handshake %s: %w", addr, dialCtx.Err())
		}
		return err
	}
	sshClient := ssh.NewClient(sshConn, chans, reqs)

	sess, err := sshClient.NewSession()
	if err != nil {
		cleanup(sshClient, nil)
		if ctx.Err() != nil {
			return fmt.Errorf("SSH session %s: %w", addr, ctx.Err())
		}
		if dialCtx.Err() != nil {
			return fmt.Errorf("SSH session %s: %w", addr, dialCtx.Err())
		}
		return err
	}

	stdin, err := sess.StdinPipe()
	if err != nil {
		cleanup(sshClient, sess)
		return err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		cleanup(sshClient, sess)
		return err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		cleanup(sshClient, sess)
		return err
	}
	if err := sess.Start(sshEndpointCommand(s.service, s.endpoint)); err != nil {
		cleanup(sshClient, sess)
		if ctx.Err() != nil {
			return fmt.Errorf("SSH start %s: %w", s.service, ctx.Err())
		}
		if dialCtx.Err() != nil {
			return fmt.Errorf("SSH start %s: %w", s.service, dialCtx.Err())
		}
		return err
	}

	stopWatch()
	if dialCtx.Err() != nil {
		cleanup(sshClient, sess)
		if ctx.Err() != nil {
			return fmt.Errorf("SSH session %s: %w", addr, ctx.Err())
		}
		return fmt.Errorf("SSH session %s: %w", addr, dialCtx.Err())
	}
	_ = conn.SetDeadline(time.Time{})

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cleanup(sshClient, sess)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return net.ErrClosed
	}
	s.conn = conn
	s.client = sshClient
	s.session = sess
	s.stdin = stdin
	s.stdout = stdout
	s.firstErrLine = sshListenFirstError(stderr)
	s.connected = true
	s.mu.Unlock()
	return nil
}

func sshEndpointCommand(cmd string, ep *transport.Endpoint) string {
	var b strings.Builder
	b.WriteString(cmd)
	b.WriteByte(' ')
	writeShellQuote(&b, ep.Path)
	return b.String()
}

// writeShellQuote wraps s in POSIX single quotes, escaping embedded single
// quotes and exclamation marks the same way canonical git and go-git do.
func writeShellQuote(b *strings.Builder, s string) {
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\'' || c == '!' {
			b.WriteString(`'\`)
			b.WriteByte(c)
			b.WriteByte('\'')
			continue
		}
		b.WriteByte(c)
	}
	b.WriteByte('\'')
}

func sshListenFirstError(r io.Reader) chan string {
	if r == nil {
		return nil
	}
	errLine := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		for {
			if sc.Scan() {
				line := sc.Text()
				if !sshStdErrSkipPattern.MatchString(line) {
					errLine <- line
					break
				}
			} else {
				close(errLine)
				break
			}
		}
		_, _ = io.Copy(io.Discard, r)
	}()
	return errLine
}

func (s *sshSession) addContextWatch(ctx context.Context) func() {
	stop := context.AfterFunc(ctx, func() { _ = s.closeTransport() })
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		stop()
		return func() {}
	}
	s.contextStops = append(s.contextStops, stop)
	s.mu.Unlock()
	return func() { stop() }
}

func (s *sshSession) AdvertisedReferences() (*packp.AdvRefs, error) {
	return s.AdvertisedReferencesContext(context.Background())
}

func (s *sshSession) AdvertisedReferencesContext(ctx context.Context) (*packp.AdvRefs, error) {
	if s.advRefs != nil {
		return s.advRefs, nil
	}
	stop := s.addContextWatch(ctx)
	defer stop()

	if err := s.ensureConnected(ctx); err != nil {
		return nil, err
	}

	ar := packp.NewAdvRefs()
	if err := ar.Decode(s.stdoutContext(ctx)); err != nil {
		if err := s.handleAdvRefDecodeError(ctx, err); err != nil {
			return nil, err
		}
	}
	if !s.isReceivePack && ar.IsEmpty() {
		return nil, transport.ErrEmptyRemoteRepository
	}
	transport.FilterUnsupportedCapabilities(ar.Capabilities)
	s.advRefs = ar
	return ar, nil
}

func (s *sshSession) handleAdvRefDecodeError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var errLine *pktline.ErrorLine
	if errors.As(err, &errLine) {
		if isSSHRepoNotFoundError(errLine.Text) {
			return transport.ErrRepositoryNotFound
		}
		return errLine
	}
	if errors.Is(err, packp.ErrEmptyInput) {
		s.mu.Lock()
		s.finished = true
		s.mu.Unlock()
		if err := s.checkNotFoundError(ctx); err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if errors.Is(err, packp.ErrEmptyAdvRefs) {
		if s.isReceivePack {
			return nil
		}
		if err := s.finish(); err != nil {
			return err
		}
		return transport.ErrEmptyRemoteRepository
	}
	var uerr *packp.ErrUnexpectedData
	if errors.As(err, &uerr) && isSSHRepoNotFoundError(string(uerr.Data)) {
		return transport.ErrRepositoryNotFound
	}
	return err
}

func (s *sshSession) UploadPack(ctx context.Context, req *packp.UploadPackRequest) (*packp.UploadPackResponse, error) {
	if req.IsEmpty() {
		if err := s.finish(); err != nil {
			return nil, err
		}
		return nil, transport.ErrEmptyUploadPackRequest
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if _, err := s.AdvertisedReferencesContext(ctx); err != nil {
		return nil, err
	}

	// Keep the cancellation watch alive after UploadPack returns so streaming
	// the packfile body from the returned UploadPackResponse is also aborted
	// immediately if ctx expires; closeTransport stops the watch on Close.
	stop := s.addContextWatch(ctx)
	keepWatching := false
	defer func() {
		if !keepWatching {
			stop()
		}
	}()

	s.mu.Lock()
	s.packRun = true
	s.mu.Unlock()

	in := s.stdinContext(ctx)
	out := s.stdoutContext(ctx)

	if err := sshUploadPack(in, req); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	r, err := ioutil.NonEmptyReader(out)
	if errors.Is(err, ioutil.ErrEmptyReader) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if c, ok := s.stdout.(io.Closer); ok {
			_ = c.Close()
		}
		return nil, transport.ErrEmptyUploadPackRequest
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}

	rc := ioutil.NewReadCloser(r, s)
	res := packp.NewUploadPackResponse(req)
	if err := res.Decode(rc); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("error decoding upload-pack response: %w", err)
	}
	keepWatching = true
	return res, nil
}

func (s *sshSession) ReceivePack(ctx context.Context, req *packp.ReferenceUpdateRequest) (*packp.ReportStatus, error) {
	if _, err := s.AdvertisedReferencesContext(ctx); err != nil {
		return nil, err
	}

	stop := s.addContextWatch(ctx)
	defer stop()

	s.mu.Lock()
	s.packRun = true
	s.mu.Unlock()

	w := s.stdinContext(ctx)
	if err := req.Encode(w); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if err := w.Close(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if !req.Capabilities.Supports(capability.ReportStatus) {
		return nil, s.closeTransport()
	}

	r := s.stdoutContext(ctx)
	var d *sideband.Demuxer
	if req.Capabilities.Supports(capability.Sideband64k) {
		d = sideband.NewDemuxer(sideband.Sideband64k, r)
	} else if req.Capabilities.Supports(capability.Sideband) {
		d = sideband.NewDemuxer(sideband.Sideband, r)
	}
	if d != nil {
		d.Progress = req.Progress
		r = d
	}

	report := packp.NewReportStatus()
	if err := report.Decode(r); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if err := report.Error(); err != nil {
		_ = s.closeTransport()
		return report, err
	}
	return report, s.closeTransport()
}

func (s *sshSession) stdinContext(ctx context.Context) io.WriteCloser {
	return ioutil.NewWriteCloserOnError(
		ioutil.NewContextWriteCloser(ctx, s.stdin),
		s.onError,
	)
}

func (s *sshSession) stdoutContext(ctx context.Context) io.Reader {
	return ioutil.NewReaderOnError(
		ioutil.NewContextReader(ctx, s.stdout),
		s.onError,
	)
}

func (s *sshSession) onError(error) {
	_ = s.closeTransport()
}

func (s *sshSession) finish() error {
	s.mu.Lock()
	connected := s.connected
	closed := s.closed
	stdin := s.stdin
	if s.finished {
		s.mu.Unlock()
		return nil
	}
	s.finished = true
	packRun := s.packRun
	s.mu.Unlock()

	if connected && !closed && !packRun && stdin != nil {
		_, err := stdin.Write(pktline.FlushPkt)
		return err
	}
	return nil
}

func (s *sshSession) Close() (err error) {
	err = s.finish()
	cerr := s.closeTransport()
	if err == nil {
		err = cerr
	}
	return err
}

func (s *sshSession) closeTransport() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	connected := s.connected
	s.connected = false
	conn := s.conn
	sess := s.session
	cl := s.client
	stops := s.contextStops
	s.contextStops = nil
	s.mu.Unlock()

	for _, stop := range stops {
		stop()
	}
	if !connected {
		return nil
	}
	// Abort the underlying socket before closing the SSH session so that
	// sess.Close() can never block on a stalled peer while Store.mu is held.
	if conn != nil {
		_ = conn.SetDeadline(time.Now())
	}
	var err error
	if cl != nil {
		err = cl.Close()
	}
	if conn != nil {
		if cerr := conn.Close(); err == nil {
			err = cerr
		}
	}
	if sess != nil {
		_ = sess.Close()
	}
	if err != nil && (errors.Is(err, net.ErrClosed) || strings.HasSuffix(err.Error(), "use of closed network connection")) {
		return nil
	}
	return err
}

func (s *sshSession) checkNotFoundError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.firstErrLine == nil {
		return nil
	}
	t := time.NewTimer(sshReadErrorTimeout)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return sshErrTimeout
	case line, ok := <-s.firstErrLine:
		if !ok || len(line) == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			return nil
		}
		if isSSHRepoNotFoundError(line) {
			return transport.ErrRepositoryNotFound
		}
		return fmt.Errorf("unknown error: %s", line)
	}
}

func isSSHRepoNotFoundError(s string) bool {
	for _, msg := range []string{
		"Repository not found.",
		"repository does not exist.",
		"does not appear to be a git repository",
		"no such repository",
		"access denied",
		"Repository does not exist or you do not have access",
		"The project you were looking for could not be found",
	} {
		if strings.Contains(s, msg) {
			return true
		}
	}
	return false
}

func sshUploadPack(w io.WriteCloser, req *packp.UploadPackRequest) error {
	if err := req.UploadRequest.Encode(w); err != nil {
		return fmt.Errorf("sending upload-req message: %w", err)
	}
	if err := req.UploadHaves.Encode(w, true); err != nil {
		return fmt.Errorf("sending haves message: %w", err)
	}
	e := pktline.NewEncoder(w)
	if err := e.Encodef("done\n"); err != nil {
		return fmt.Errorf("sending done message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing input: %w", err)
	}
	return nil
}
