package service

import "testing"

func TestParseBearerChallenge(t *testing.T) {
	realm, params, err := parseBearerChallenge(
		`Bearer realm="https://ghcr.io/token",service="ghcr.io",scope="repository:alex-mabrouk/okdp-packages/rustfs:pull"`,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if realm != "https://ghcr.io/token" {
		t.Errorf("realm = %q", realm)
	}
	if got := params.Get("service"); got != "ghcr.io" {
		t.Errorf("service = %q", got)
	}
	if got := params.Get("scope"); got != "repository:alex-mabrouk/okdp-packages/rustfs:pull" {
		t.Errorf("scope = %q", got)
	}
}

func TestParseBearerChallengeRejectsBasic(t *testing.T) {
	if _, _, err := parseBearerChallenge(`Basic realm="registry"`); err == nil {
		t.Fatal("expected an error for a non-Bearer challenge")
	}
}

func TestParseBearerChallengeRequiresRealm(t *testing.T) {
	if _, _, err := parseBearerChallenge(`Bearer service="ghcr.io"`); err == nil {
		t.Fatal("expected an error when the challenge has no realm")
	}
}

func TestTokenRealmMustBeOnTheRegistryHostOverHTTPS(t *testing.T) {
	for _, tc := range []struct {
		realm, registry string
		insecure, ok    bool
	}{
		{"https://ghcr.io/token", "ghcr.io", false, true},
		{"https://quay.io/v2/auth", "quay.io", false, true},
		{"https://auth.docker.io/token", "registry-1.docker.io", false, true},
		{"https://harbor.example:8443/service/token", "harbor.example", false, true},
		{"http://registry:5000/token", "registry:5000", true, true},
		{"http://ghcr.io/token", "ghcr.io", false, false},
		{"https://169.254.169.254/latest/meta-data", "ghcr.io", false, false},
		{"https://auth.docker.io/token", "ghcr.io", false, false},
		{"https://ghcr.io.evil.example/token", "ghcr.io", false, false},
		{"file:///etc/passwd", "ghcr.io", false, false},
		{"/token", "ghcr.io", false, false},
	} {
		_, err := checkTokenRealm(tc.realm, tc.registry, tc.insecure)
		if (err == nil) != tc.ok {
			t.Errorf("realm %q for registry %q (insecure %v): err = %v, want ok = %v", tc.realm, tc.registry, tc.insecure, err, tc.ok)
		}
	}
}
