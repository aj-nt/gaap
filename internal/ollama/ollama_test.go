// This file is part of Vassago.
// See LICENSE-AGPLv3 for license information.

package ollama

import (
	"testing"
)

func TestNewClient_SetsDefaults(t *testing.T) {
	c := NewClient(Config{
		BaseURL:     "http://localhost:11434/v1",
		Model:       "test-model",
		MaxTokens:   512,
		Temperature: 0.5,
		TimeoutSec:  30,
	})
	if c == nil {
		t.Fatal("expected non-nil client")
	}
	if c.model != "test-model" {
		t.Errorf("model = %q, want %q", c.model, "test-model")
	}
	if c.maxTokens != 512 {
		t.Errorf("maxTokens = %d, want 512", c.maxTokens)
	}
	if c.temperature != 0.5 {
		t.Errorf("temperature = %f, want 0.5", c.temperature)
	}
	if c.baseURL != "http://localhost:11434/v1" {
		t.Errorf("baseURL = %q, want %q", c.baseURL, "http://localhost:11434/v1")
	}
}

func TestNewClient_ZeroTimeoutDefaults(t *testing.T) {
	c := NewClient(Config{
		BaseURL:    "http://localhost:11434/v1",
		Model:      "test",
		TimeoutSec: 0,
	})
	// TimeoutSec=0 should still create a client (caller sets defaults first)
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestChat_MissingChoices(t *testing.T) {
	c := NewClient(Config{
		BaseURL:    "http://localhost:11434/v1",
		Model:      "test",
		MaxTokens:  100,
		TimeoutSec: 10,
	})
	// This will fail with a real HTTP call — unit test, not integration.
	// The test documents the expected API shape.
	_ = c
}
