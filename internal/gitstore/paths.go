package gitstore

import (
	"fmt"
	"path"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Canonical layout under the store path. One file per object, projects
// under projects/ so that no project name collides with a top-level
// directory:
//
//	contexts/<name>.yaml
//	projects/<project>/namespace.yaml
//	projects/<project>/releases/<name>.yaml
//	projects/<project>/connections/<name>.yaml
//	projects/<project>/secret-stores/<name>.yaml
//	projects/<project>/external-secrets/<name>.yaml
const (
	contextsDir        = "contexts"
	projectsDir        = "projects"
	namespaceFile      = "namespace.yaml"
	releasesDir        = "releases"
	connectionsDir     = "connections"
	secretStoresDir    = "secret-stores"
	externalSecretsDir = "external-secrets"
	objectExt          = ".yaml"
)

// ContextsDir is the directory holding the control plane Contexts.
func ContextsDir() string { return contextsDir }

// ControlPlaneContextFile is the file holding the control plane Context
// name, a Kubernetes object name (DNS-1123 subdomain).
func ControlPlaneContextFile(name string) (string, error) {
	if err := checkObjectName(name); err != nil {
		return "", err
	}
	return contextsDir + "/" + name + objectExt, nil
}

// ProjectDir is the directory of a project. The project is a namespace name,
// a DNS-1123 label.
func ProjectDir(project string) (string, error) {
	if errs := validation.IsDNS1123Label(project); len(errs) > 0 {
		return "", fmt.Errorf("%w: project %q: %s", ErrInvalidPath, project, strings.Join(errs, ", "))
	}
	return projectsDir + "/" + project, nil
}

// NamespaceFile is the file holding the Namespace of a project.
func NamespaceFile(project string) (string, error) {
	dir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	return dir + "/" + namespaceFile, nil
}

// ReleasesDir is the directory holding the Releases of a project.
func ReleasesDir(project string) (string, error) {
	return projectSubdir(project, releasesDir)
}

// ConnectionsDir is the directory holding the Connections of a project.
func ConnectionsDir(project string) (string, error) {
	return projectSubdir(project, connectionsDir)
}

// SecretStoresDir is the directory holding the SecretStores of a project.
func SecretStoresDir(project string) (string, error) {
	return projectSubdir(project, secretStoresDir)
}

// ExternalSecretsDir is the directory holding the ExternalSecrets of a project.
func ExternalSecretsDir(project string) (string, error) {
	return projectSubdir(project, externalSecretsDir)
}

// ReleaseFile is the file holding the Release name of a project. The name is
// a Kubernetes object name, a DNS-1123 subdomain.
func ReleaseFile(project, name string) (string, error) {
	return objectFile(project, releasesDir, name)
}

// ConnectionFile is the file holding the Connection name of a project.
func ConnectionFile(project, name string) (string, error) {
	return objectFile(project, connectionsDir, name)
}

// SecretStoreFile is the file holding the SecretStore name of a project.
func SecretStoreFile(project, name string) (string, error) {
	return objectFile(project, secretStoresDir, name)
}

// ExternalSecretFile is the file holding the ExternalSecret name of a project.
func ExternalSecretFile(project, name string) (string, error) {
	return objectFile(project, externalSecretsDir, name)
}

// ObjectName returns the object name of a file listed in an object
// directory (contexts, releases, connections, secret-stores,
// external-secrets), and false for any other entry.
func ObjectName(fileName string) (string, bool) {
	name, ok := strings.CutSuffix(fileName, objectExt)
	if !ok || checkObjectName(name) != nil {
		return "", false
	}
	return name, true
}

func projectSubdir(project, dir string) (string, error) {
	projectDir, err := ProjectDir(project)
	if err != nil {
		return "", err
	}
	return projectDir + "/" + dir, nil
}

func objectFile(project, dir, name string) (string, error) {
	sub, err := projectSubdir(project, dir)
	if err != nil {
		return "", err
	}
	if err := checkObjectName(name); err != nil {
		return "", err
	}
	return sub + "/" + name + objectExt, nil
}

func checkObjectName(name string) error {
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("%w: name %q: %s", ErrInvalidPath, name, strings.Join(errs, ", "))
	}
	return nil
}

// cleanPath checks a slash-separated path relative to the store root and
// returns it unchanged. It refuses anything that could reach outside the
// root or into git internals instead of normalising it: a caller passing
// "a/../b" has a bug worth seeing.
func cleanPath(p string, allowEmpty bool) (string, error) {
	if p == "" {
		if allowEmpty {
			return "", nil
		}
		return "", fmt.Errorf("%w: empty path", ErrInvalidPath)
	}
	if strings.ContainsFunc(p, func(r rune) bool { return r == '\\' || r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("%w: %q contains a backslash or a control character", ErrInvalidPath, p)
	}
	if strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return "", fmt.Errorf("%w: %q must be relative, without trailing slash", ErrInvalidPath, p)
	}
	for _, part := range strings.Split(p, "/") {
		switch {
		case part == "", part == ".", part == "..":
			return "", fmt.Errorf("%w: %q has an empty, . or .. component", ErrInvalidPath, p)
		case strings.EqualFold(part, ".git"):
			return "", fmt.Errorf("%w: %q has a .git component", ErrInvalidPath, p)
		}
	}
	if path.Clean(p) != p {
		return "", fmt.Errorf("%w: %q is not a clean path", ErrInvalidPath, p)
	}
	return p, nil
}

// join puts a checked relative path under the store root.
func join(root, rel string) string {
	switch {
	case root == "":
		return rel
	case rel == "":
		return root
	default:
		return root + "/" + rel
	}
}
