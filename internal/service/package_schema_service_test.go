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
