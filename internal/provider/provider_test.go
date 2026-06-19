package provider_test

import (
	"strings"
	"testing"

	"github.com/tyler/podcast-migrate/internal/provider"
)

// ---- ErrCapabilityUnsupported ----

func TestErrCapabilityUnsupported_ErrorString(t *testing.T) {
	err := &provider.ErrCapabilityUnsupported{
		Provider:  "Apple Podcasts",
		Operation: "write subscriptions",
	}
	msg := err.Error()
	if !strings.Contains(msg, "Apple Podcasts") {
		t.Errorf("error should mention provider name: %q", msg)
	}
	if !strings.Contains(msg, "write subscriptions") {
		t.Errorf("error should mention operation: %q", msg)
	}
	if !strings.Contains(msg, "not supported") {
		t.Errorf("error should say 'not supported': %q", msg)
	}
}

func TestErrCapabilityUnsupported_ImplementsError(t *testing.T) {
	var _ error = &provider.ErrCapabilityUnsupported{}
}

func TestErrCapabilityUnsupported_EmptyFields(t *testing.T) {
	err := &provider.ErrCapabilityUnsupported{}
	// Must not panic and must return a non-empty string.
	msg := err.Error()
	if msg == "" {
		t.Error("Error() should return non-empty string even with empty fields")
	}
}

// ---- Capabilities zero value ----

func TestCapabilities_ZeroValue_AllFalse(t *testing.T) {
	var caps provider.Capabilities
	if caps.ReadSubscriptions || caps.WriteSubscriptions || caps.ReadPlayState || caps.WritePlayState {
		t.Error("zero-value Capabilities should have all fields false")
	}
}
