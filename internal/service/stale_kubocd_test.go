package service

import (
	"strings"
	"testing"
)

// The two failures a kubocd older than the unified model produces. Both read as
// a broken package, which sends the reader editing a file that is fine.
func TestStaleKubocdIsNamed(t *testing.T) {
	outputs := []string{
		"ERROR: invalid 'schema.parameters': unknown type 'connectionRef' for node '.metadataDb'",
		`ERROR: error unmarshaling JSON: while decoding JSON: json: unknown field "outputs"`,
	}

	for _, output := range outputs {
		message := staleKubocd(output)
		if message == "" {
			t.Fatalf("expected %q to be recognised as a stale binary", output)
		}
		if !strings.Contains(message, "kubocd binary shipped with this server") {
			t.Errorf("expected the message to blame the server, got %q", message)
		}
	}
}

// Anything else is a real package problem and must keep its own message.
func TestOtherFailuresAreLeftAlone(t *testing.T) {
	for _, output := range []string{
		"ERROR: failed to pull image: not found",
		"",
		"unknown type 'somethingElse'",
	} {
		if message := staleKubocd(output); message != "" {
			t.Errorf("expected %q to pass through, got %q", output, message)
		}
	}
}
