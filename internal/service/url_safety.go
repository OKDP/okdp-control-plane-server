package service

import (
	"net/url"
	"strings"
)

// sensitiveQueryParams lists normalized (lowercase, '_' and '-' stripped)
// parameter keys that commonly carry inline credentials and must never appear
// in public URL fields persisted in Kubernetes CRs or Git.
var sensitiveQueryParams = map[string]bool{
	"password":              true,
	"passwd":                true,
	"pwd":                   true,
	"pass":                  true,
	"token":                 true,
	"accesstoken":           true,
	"authtoken":             true,
	"privatetoken":          true,
	"secret":                true,
	"secretkey":             true,
	"accesskey":             true,
	"apikey":                true,
	"clientsecret":          true,
	"privatekey":            true,
	"sessiontoken":          true,
	"sslpassword":           true,
	"sslkeystorepassword":   true,
	"ssltruststorepassword": true,
	"xamzsecuritytoken":     true,
	"xamzsignature":         true,
}

const maxURLUnwrapDepth = 8

// hasEmbeddedURLCredentials reports whether raw carries inline credentials —
// either URL userinfo (including compound schemes, passwords containing '/',
// '?' or '#', or URLs preceded by '/'), sensitive query or ';'-delimited path
// parameters, JDBC thin credentials, or schemeless user:pass@host forms.
// Public fields persisted in Kubernetes CRs or Git must never carry inline
// credentials.
func hasEmbeddedURLCredentials(raw string) bool {
	return checkURLCredentials(raw, 0)
}

func checkURLCredentials(raw string, depth int) bool {
	if depth > maxURLUnwrapDepth {
		// Fail-closed: refuse excessively nested scheme wrappers.
		return true
	}
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}

	hasSchemeSep := strings.Contains(s, "://")
	isProtoRel := strings.HasPrefix(s, "//")
	if !hasSchemeSep && !isProtoRel {
		return checkSchemelessCredentials(s)
	}

	rest := s
	if hasSchemeSep {
		_, rest, _ = strings.Cut(s, "://")
	} else {
		rest = strings.TrimPrefix(s, "//")
	}

	u, err := url.Parse(s)
	if err != nil {
		// Fail-closed: inspect raw authority, split-password patterns, query
		// parameters, and ';'-delimited parameters (e.g. jdbc:sqlserver).
		if hasAuthorityOrSplitCredentials(rest, true) {
			return true
		}
		if hasRawSensitiveParams(rest) {
			return true
		}
		if nextIdx := strings.Index(rest, "://"); nextIdx >= 0 {
			if checkURLCredentials("//"+rest[nextIdx+3:], depth+1) {
				return true
			}
		}
		return false
	}

	if u.User != nil {
		return true
	}
	// When '/' precedes "://" (e.g. "foo/https://user:pass@host"), url.Parse
	// parses the whole string as a relative path with an empty scheme. Unwrap
	// from "//" so the embedded URL is inspected.
	if u.Scheme == "" && hasSchemeSep {
		return checkURLCredentials("//"+rest, depth+1)
	}
	if hasAuthorityOrSplitCredentials(rest, false) {
		return true
	}
	if u.RawQuery != "" && hasSensitiveParams(u.RawQuery, false) {
		return true
	}
	if _, semi, ok := strings.Cut(u.Path, ";"); ok && hasSensitiveParams(semi, true) {
		return true
	}
	if u.Fragment != "" && hasSensitiveParams(u.Fragment, true) {
		return true
	}
	if u.Opaque != "" && (strings.Contains(u.Opaque, "://") || strings.HasPrefix(u.Opaque, "//")) {
		return checkURLCredentials(u.Opaque, depth+1)
	}
	if nextIdx := strings.Index(rest, "://"); nextIdx >= 0 {
		if checkURLCredentials("//"+rest[nextIdx+3:], depth+1) {
			return true
		}
	}
	return false
}

// hasAuthorityOrSplitCredentials checks rest (the text following "://" or "//")
// for userinfo '@' in the authority, or for passwords split across '/', '?', or
// '#' (such as "admin:p/ass@host:8200" or "admin:123/secret@host:8200").
func hasAuthorityOrSplitCredentials(rest string, allowSpaceInAuthority bool) bool {
	if allowSpaceInAuthority {
		authority := rest
		if idx := strings.IndexAny(rest, "/?#"); idx >= 0 {
			authority = rest[:idx]
		}
		if strings.Contains(authority, "@") {
			return true
		}
	}

	token := rest
	if ws := strings.IndexAny(token, " \t\r\n"); ws >= 0 {
		token = token[:ws]
	}
	authority := token
	if sep := strings.IndexAny(token, "/?#"); sep >= 0 {
		authority = token[:sep]
	}
	if strings.Contains(authority, "@") {
		return true
	}
	if !strings.HasPrefix(authority, "[") && strings.Contains(authority, ":") {
		if at := strings.LastIndexByte(token, '@'); at >= 0 && !isOCIDigestSuffix(token[at+1:]) {
			return true
		}
	}
	return false
}

// hasRawSensitiveParams inspects '?' query strings, ';'-delimited parameters
// (e.g. jdbc:sqlserver://host:1433;password=secret), and '#' fragments on a raw
// URL substring when url.Parse failed or on schemeless JDBC URLs.
func hasRawSensitiveParams(rest string) bool {
	beforeFrag, frag, hasFrag := strings.Cut(rest, "#")
	if hasFrag && hasSensitiveParams(frag, true) {
		return true
	}
	beforeQ, query, hasQ := strings.Cut(beforeFrag, "?")
	if hasQ && hasSensitiveParams(query, false) {
		return true
	}
	if _, semi, hasSemi := strings.Cut(beforeQ, ";"); hasSemi && hasSensitiveParams(semi, true) {
		return true
	}
	return false
}

// checkSchemelessCredentials detects inline credentials in strings without
// "://", such as "user:SuperSecret@s3.example.com:9000" or
// "jdbc:oracle:thin:scott/tiger@//db:1521/orcl", while allowing plain emails
// ("adm@okdp.io"), SSH user@host refs without passwords, OCI image digests
// ("quay.io/okdp/pkg:1.0@sha256:..."), and benign text.
func checkSchemelessCredentials(s string) bool {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "jdbc:") {
		if hasRawSensitiveParams(s) {
			return true
		}
		if at := strings.LastIndexByte(s, '@'); at > 0 {
			beforeAt := s[:at]
			if lastColon := strings.LastIndexByte(beforeAt, ':'); lastColon >= 0 {
				if strings.TrimSpace(beforeAt[lastColon+1:]) != "" {
					return true
				}
			}
		}
		return false
	}

	for _, tok := range strings.Fields(s) {
		at := strings.LastIndexByte(tok, '@')
		if at <= 0 || at >= len(tok)-1 {
			continue
		}
		beforeAt, afterAt := tok[:at], tok[at+1:]
		if isOCIDigestSuffix(afterAt) {
			continue
		}
		if strings.HasPrefix(strings.ToLower(beforeAt), "mailto:") {
			if strings.Contains(beforeAt[len("mailto:"):], ":") {
				return true
			}
			continue
		}
		user, _, hasColon := strings.Cut(beforeAt, ":")
		if !hasColon || user == "" || strings.ContainsAny(user, "/?#") {
			continue
		}
		if looksLikeEndpoint(afterAt) {
			return true
		}
	}
	return false
}

func isOCIDigestSuffix(afterAt string) bool {
	lower := strings.ToLower(afterAt)
	return strings.HasPrefix(lower, "sha256:") ||
		strings.HasPrefix(lower, "sha384:") ||
		strings.HasPrefix(lower, "sha512:")
}

func looksLikeEndpoint(afterAt string) bool {
	if afterAt == "" {
		return false
	}
	c := afterAt[0]
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '[' || c == '/'
}

func hasSensitiveParams(raw string, requireEquals bool) bool {
	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == '&' || r == ';'
	})
	for _, part := range parts {
		if part == "" {
			continue
		}
		key, _, hasEq := strings.Cut(part, "=")
		if requireEquals && !hasEq {
			continue
		}
		if sensitiveQueryParams[normalizeParamKey(key)] {
			return true
		}
	}
	return false
}

func normalizeParamKey(key string) string {
	if unescaped, err := url.QueryUnescape(key); err == nil {
		key = unescaped
	}
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "")
	key = strings.ReplaceAll(key, "-", "")
	return key
}
