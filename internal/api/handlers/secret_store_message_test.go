package handlers

import (
	"strings"
	"testing"

	"github.com/okdp/okdp-control-plane-server/internal/models"
)

// A Kubernetes-auth store cannot present a token, so the test only proves the
// server answers. Saying "successful" alone let a wrong role look verified.
func TestConnectionMessageNamesWhatKubernetesAuthLeavesUnverified(t *testing.T) {
	req := models.SecretStoreRequest{Auth: &models.SecretStoreAuth{Type: "kubernetes"}}
	got := testConnectionMessage(req)
	if !strings.Contains(got, "operator") || !strings.Contains(got, "role") {
		t.Fatalf("got %q, want a message that says the role is verified by the operator", got)
	}
}

func TestConnectionMessageStaysPlainForTokenAuth(t *testing.T) {
	req := models.SecretStoreRequest{Auth: &models.SecretStoreAuth{Type: "token"}}
	if got := testConnectionMessage(req); got != "Connection successful" {
		t.Fatalf("got %q, want the plain success", got)
	}
}
