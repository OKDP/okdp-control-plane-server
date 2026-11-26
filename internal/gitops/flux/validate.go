package flux

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Validate checks that the GitRepository and the Kustomization exist, that
// the Kustomization applies that GitRepository with a ServiceAccount of its
// own, and that together they pull ExpectedRepository: same URL, branch and
// path. It reports every mismatch.
func (a *Adapter) Validate(ctx context.Context) error {
	repo, err := a.repos().Get(ctx, a.cfg.GitRepositoryName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("flux: read %s: %w", a.repoRef(), describeGetError(err))
	}
	ks, err := a.kustomizations().Get(ctx, a.cfg.KustomizationName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("flux: read %s: %w", a.ksRef(), describeGetError(err))
	}

	want := a.cfg.ExpectedRepository
	var errs []error
	errs = append(errs, a.checkSourceRef(ks)...)
	errs = append(errs, a.checkURL(repo, want.URL)...)
	errs = append(errs, a.checkRef(repo, want.Branch)...)

	if sa, _, _ := unstructured.NestedString(ks.Object, "spec", "serviceAccountName"); sa == "" {
		errs = append(errs, fmt.Errorf("%s has no spec.serviceAccountName: it would apply the branch with the rights of the kustomize-controller, set a ServiceAccount limited to what the server commits", a.ksRef()))
	}
	if prune, _, _ := unstructured.NestedBool(ks.Object, "spec", "prune"); !prune {
		errs = append(errs, fmt.Errorf("%s has spec.prune=%t: must be true so deletions in git prune objects from the cluster", a.ksRef(), prune))
	}
	if targetNS, _, _ := unstructured.NestedString(ks.Object, "spec", "targetNamespace"); targetNS != "" {
		errs = append(errs, fmt.Errorf("%s sets spec.targetNamespace %q: must be empty so manifests apply to their declared namespaces", a.ksRef(), targetNS))
	}
	if kubeConfig, found, _ := unstructured.NestedFieldNoCopy(ks.Object, "spec", "kubeConfig"); found && kubeConfig != nil {
		errs = append(errs, fmt.Errorf("%s sets spec.kubeConfig: applying to a remote cluster is not supported", a.ksRef()))
	}

	gotPath, _, _ := unstructured.NestedString(ks.Object, "spec", "path")
	if hasParentSegment(gotPath) {
		errs = append(errs, fmt.Errorf("%s has spec.path %q containing a \"..\" segment", a.ksRef(), gotPath))
	} else if normalizePath(gotPath) != normalizePath(want.Path) {
		errs = append(errs, fmt.Errorf("%s applies spec.path %q, the server commits to %q",
			a.ksRef(), gotPath, want.Path))
	}

	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("flux: the sync objects do not match the server repository: %w", err)
	}
	return nil
}

func describeGetError(err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("not found: %w", err)
	}
	return err
}

func (a *Adapter) checkSourceRef(ks *unstructured.Unstructured) []error {
	kind, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "kind")
	name, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "name")
	ns, _, _ := unstructured.NestedString(ks.Object, "spec", "sourceRef", "namespace")
	if ns == "" {
		ns = ks.GetNamespace()
	}
	if kind == gitRepositoryKind && name == a.cfg.GitRepositoryName && ns == a.cfg.Namespace {
		return nil
	}
	return []error{fmt.Errorf("%s has spec.sourceRef %s %s/%s, want %s",
		a.ksRef(), kind, ns, name, a.repoRef())}
}

func (a *Adapter) checkURL(repo *unstructured.Unstructured, want string) []error {
	got, _, _ := unstructured.NestedString(repo.Object, "spec", "url")
	wantN, wantErr := normalizeURL(want)
	if wantErr != nil {
		return []error{fmt.Errorf("expected repository URL: %w", wantErr)}
	}
	if err := checkURLSafe(got); err != nil {
		return []error{fmt.Errorf("%s spec.url: %w", a.repoRef(), err)}
	}
	gotN, gotErr := normalizeURL(got)
	switch {
	case gotErr != nil:
		return []error{fmt.Errorf("%s spec.url: %w", a.repoRef(), gotErr)}
	case gotN.url != wantN.url:
		return []error{fmt.Errorf("%s pulls %s, the server commits to %s",
			a.repoRef(), gotN.quote(), wantN.quote())}
	}
	return nil
}

// checkURLSafe rejects a GitRepository spec.url that carries embedded
// credentials (any password on any scheme, any userinfo or '@' on HTTP/HTTPS)
// or a query string, without ever quoting the raw URL.
func checkURLSafe(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return nil
	}
	if strings.Contains(s, "?") {
		return errors.New("URL must not contain a query string")
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			if strings.Contains(s, "@") {
				return errors.New("URL must not embed credentials")
			}
			return errors.New("not a valid git URL")
		}
		if u.RawQuery != "" {
			return errors.New("URL must not contain a query string")
		}
		scheme := strings.ToLower(u.Scheme)
		if scheme == "http" || scheme == "https" {
			if u.User != nil || strings.Contains(s, "@") {
				return errors.New("URL must not embed credentials")
			}
			return nil
		}
		if u.User != nil {
			if _, hasPass := u.User.Password(); hasPass {
				return errors.New("URL must not embed a password")
			}
		}
		if strings.Contains(u.Host, ":") && !strings.HasPrefix(u.Host, "[") &&
			(strings.Contains(u.EscapedPath(), "@") || strings.Contains(u.EscapedFragment(), "@")) {
			return errors.New("URL must not embed a password")
		}
		return nil
	}
	if firstAt := strings.IndexByte(s, '@'); firstAt >= 0 && strings.Contains(s[:firstAt], ":") {
		return errors.New("URL must not embed a password")
	}
	return nil
}

// checkRef requires spec.ref.branch to be the server branch and no other ref
// field to be set: Flux gives commit, name, semver and tag precedence over
// branch, and defaults to its own branch when ref is absent.
func (a *Adapter) checkRef(repo *unstructured.Unstructured, want string) []error {
	var errs []error
	for _, field := range []string{"commit", "name", "semver", "tag"} {
		if v, _, _ := unstructured.NestedString(repo.Object, "spec", "ref", field); v != "" {
			errs = append(errs, fmt.Errorf("%s pins spec.ref.%s %q, the server commits to branch %q",
				a.repoRef(), field, v, want))
		}
	}
	branch, _, _ := unstructured.NestedString(repo.Object, "spec", "ref", "branch")
	switch {
	case branch == "":
		errs = append(errs, fmt.Errorf("%s has no spec.ref.branch, want %q", a.repoRef(), want))
	case branch != want:
		errs = append(errs, fmt.Errorf("%s follows branch %q, the server commits to %q",
			a.repoRef(), branch, want))
	}
	return errs
}

// normalizedURL is a git URL reduced for comparison, without user info.
type normalizedURL struct {
	url string
	// quotable is false when user info may have been read as host, path,
	// query or fragment, so that url could hold credentials.
	quotable bool
}

// quote returns the URL for an error message, or a placeholder when it may
// hold credentials.
func (n normalizedURL) quote() string {
	if n.quotable {
		return n.url
	}
	return "a URL not shown since it may hold credentials"
}

// normalizeURL reduces a git URL to scheme://host[:port]/path for comparison:
// scp-like user@host:path becomes ssh://host/path, user info and default
// ports are dropped, scheme and host are lower-cased, a trailing slash and a
// .git suffix are removed. The path keeps its case. An error never quotes
// raw, which may hold credentials that url.Parse could not separate.
func normalizeURL(raw string) (normalizedURL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return normalizedURL{}, errors.New("empty URL")
	}
	if !strings.Contains(s, "://") {
		host, p, ok := strings.Cut(s, ":")
		if !ok || strings.Contains(host, "/") {
			return normalizedURL{}, errors.New("not a valid git URL")
		}
		s = "ssh://" + host + "/" + strings.TrimLeft(p, "/")
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return normalizedURL{}, errors.New("not a valid git URL")
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if port := u.Port(); port != "" && port != defaultPorts[scheme] {
		host += ":" + port
	}
	p := "/" + strings.TrimLeft(u.Path, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimSuffix(p, ".git")
	p = strings.TrimSuffix(p, "/")
	// User info needs an "@". With none, or with a single one that url.Parse
	// took as the end of the user info, nothing else can hold credentials.
	ats := strings.Count(s, "@")
	return normalizedURL{
		url:      scheme + "://" + host + p,
		quotable: ats == 0 || (ats == 1 && u.User != nil),
	}, nil
}

var defaultPorts = map[string]string{"http": "80", "https": "443", "ssh": "22"}

// hasParentSegment reports whether p contains any ".." path component.
func hasParentSegment(p string) bool {
	for _, seg := range strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' }) {
		if seg == ".." {
			return true
		}
	}
	return false
}

// normalizePath maps the forms Flux accepts for one directory to the form of
// gitops.Repository.Path: "./apps/" and "apps" give "apps", "." and "./" give "".
func normalizePath(p string) string {
	return strings.TrimPrefix(path.Clean("/"+p), "/")
}
