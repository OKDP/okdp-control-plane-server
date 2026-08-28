package service

import "strings"

// insecureOCIHost reports whether the repository lives on one of the
// registries declared plain-HTTP through INSECURE_OCI_REGISTRIES.
//
// This exists for development sandboxes, where packages are pushed to a local
// registry with no TLS: the chart schema pull and the tag listing must be told to
// speak plain HTTP, or every fetch dies on a handshake. Production registries
// are never listed, so the default behaviour stays strict HTTPS.
func insecureOCIHost(repository string, insecureHosts []string) bool {
	host := strings.TrimPrefix(repository, "oci://")
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
