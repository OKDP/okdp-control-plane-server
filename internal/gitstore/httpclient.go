package gitstore

import (
	"errors"
	"net/http"
	"strings"

	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// go-git picks its HTTP transport from a process-wide table. It is replaced
// here once, before any goroutine can read it, by a client that never
// follows a redirect to another origin, so that no Authorization header
// can reach another host or downgrade to plain HTTP.
func init() {
	c := githttp.NewClientWithOptions(&http.Client{
		// go-git clones this transport to add a CA bundle, so it must be an
		// *http.Transport. The clone keeps the proxy from the environment.
		Transport:     http.DefaultTransport.(*http.Transport).Clone(),
		CheckRedirect: sameOriginRedirect,
	}, &githttp.ClientOptions{
		CacheMaxEntries: 8,
	})
	client.InstallProtocol("http", c)
	client.InstallProtocol("https", c)
}

var errCrossOriginRedirect = errors.New("git remote redirected to another origin (scheme, host or port), refused")

// sameOriginRedirect allows a redirect only within the origin of the first
// request. The error does not quote the URLs.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) == 0 {
		return nil
	}
	if origin(req) != origin(via[0]) {
		return errCrossOriginRedirect
	}
	if len(via) >= 10 {
		return errors.New("git remote redirected too many times")
	}
	return nil
}

func origin(r *http.Request) string {
	scheme := strings.ToLower(r.URL.Scheme)
	port := r.URL.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme + "://" + strings.ToLower(r.URL.Hostname()) + ":" + port
}
