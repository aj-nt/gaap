// This file is part of Vassago.
// See LICENSE-AGPLv3 for license information.

package ollama

import (
	"net/http"
	"net/http/httptest"
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
	if c == nil {
		t.Fatal("expected non-nil client")
	}
}

func TestChat_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected /chat/completions, got %s", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %s", ct)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"Hello from test"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	resp, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "Hello from test" {
		t.Errorf("resp = %q, want %q", resp, "Hello from test")
	}
}

func TestChat_Non200StatusCode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"internal server error"}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	_, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for non-200 status code")
	}
}

func TestChat_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	_, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestChat_EmptyChoices(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	_, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for empty choices")
	}
	if err.Error() != "no choices in response" {
		t.Errorf("err = %q, want %q", err.Error(), "no choices in response")
	}
}

func TestChat_EmptyContentWithReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","reasoning":"I think the answer is 42"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	resp, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "I think the answer is 42" {
		t.Errorf("resp = %q, want %q", resp, "I think the answer is 42")
	}
}

func TestChat_EmptyContentWithReasoningContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"DeepSeek reasoning here"}}]}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	resp, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp != "DeepSeek reasoning here" {
		t.Errorf("resp = %q, want %q", resp, "DeepSeek reasoning here")
	}
}

func TestChat_EmptyContentNoReasoning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""}}]}`))
	}))
	defer srv.Close()

	c := NewClient(Config{
		BaseURL:    srv.URL,
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 10,
	})

	_, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for empty content with no reasoning")
	}
	if err.Error() != "empty response content" {
		t.Errorf("err = %q, want %q", err.Error(), "empty response content")
	}
}

func TestModel(t *testing.T) {
	c := NewClient(Config{
		BaseURL:    "http://localhost:11434/v1",
		Model:      "claude-sonnet-4",
		TimeoutSec: 10,
	})
	if got := c.Model(); got != "claude-sonnet-4" {
		t.Errorf("Model() = %q, want %q", got, "claude-sonnet-4")
	}
}

func TestChat_UnreachableServer(t *testing.T) {
	c := NewClient(Config{
		BaseURL:    "http://127.0.0.1:19999",
		Model:      "test-model",
		MaxTokens:  100,
		TimeoutSec: 1,
	})

	_, err := c.Chat([]Message{{Role: "user", Content: "hello"}})
	if err == nil {
		t.Fatal("expected error for unreachable server")
	}
}
