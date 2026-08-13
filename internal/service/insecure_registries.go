package service

import "strings"

// insecureOCIHost reports whether the repository lives on one of the
// registries declared plain-HTTP through INSECURE_OCI_REGISTRIES.
//
// This exists for development sandboxes, where packages are pushed to a local
// registry with no TLS: the schema dump and the Release must both be told to
// speak plain HTTP, or every fetch dies on a handshake. Production registries
// are never listed, so the default behaviour stays strict HTTPS.
func insecureOCIHost(repository string, insecureHosts []string) bool {
	host := repository
	if i := strings.IndexByte(host, '/'); i >= 0 {
		host = host[:i]
	}
	for _, candidate := range insecureHosts {
		if strings.TrimSpace(candidate) == host {
			return true
		}
	}
	return false
}
