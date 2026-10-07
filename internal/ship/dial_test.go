package ship

import (
	"strings"
	"testing"
)

func TestDialRefusesPlaintextToAnotherMachine(t *testing.T) {
	for hp, want := range map[string]bool{"localhost:7233": true, "127.0.0.1:7233": true, "[::1]:7233": true, "10.0.0.5:7233": false, "temporal.example.com:7233": false, "localhost": false} {
		if got := loopback(hp); got != want {
			t.Errorf("loopback(%q) = %v, want %v", hp, got, want)
		}
	}
	t.Setenv("TEMPORAL_ADDRESS", "temporal.example.com:7233")
	t.Setenv("TEMPORAL_API_KEY", "")
	if _, err := Dial(); err == nil || !strings.Contains(err.Error(), "not this machine") {
		t.Errorf("Dial to a remote host without a key: err = %v", err)
	}
}
