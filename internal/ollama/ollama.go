// This file is part of Vassago.
// See LICENSE-AGPLv3 for license information.

// Package ollama provides a minimal OpenAI-compatible client for Ollama LLM backends.
// Used by workers and the orchestrator for LLM-powered task execution and
// goal decomposition.
package ollama

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Client is a minimal OpenAI-compatible HTTP client for Ollama.
type Client struct {
	baseURL     string
	model       string
	maxTokens   int
	temperature float64
	timeout     time.Duration
	client      *http.Client
}

// Config holds the configuration for an Ollama client.
type Config struct {
	BaseURL     string  `yaml:"base_url"`
	Model       string  `yaml:"model"`
	MaxTokens   int     `yaml:"max_tokens"`
	Temperature float64 `yaml:"temperature"`
	TimeoutSec  int     `yaml:"timeout_sec"`
}

// NewClient creates a new Ollama client from the given config.
func NewClient(cfg Config) *Client {
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &Client{
		baseURL:     cfg.BaseURL,
		model:       cfg.Model,
		maxTokens:   cfg.MaxTokens,
		temperature: cfg.Temperature,
		timeout:     timeout,
		client:      &http.Client{Timeout: timeout},
	}
}

// Message represents a single chat message.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// chatRequest is the JSON body sent to the /chat/completions endpoint.
type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
}

// chatResponse is the JSON response from the /chat/completions endpoint.
type chatResponse struct {
	Choices []Choice `json:"choices"`
}

// Choice represents a single completion choice.
type Choice struct {
	Message Message `json:"message"`
}

// Chat sends a chat completion request and returns the response text.
func (c *Client) Chat(messages []Message) (string, error) {
	req := chatRequest{
		Model:       c.model,
		Messages:    messages,
		MaxTokens:   c.maxTokens,
		Temperature: c.temperature,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequest("POST", c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != 200 {
		end := min(len(respBody), 500)
		return "", fmt.Errorf("http %d: %s", resp.StatusCode, string(respBody[:end]))
	}

	var cr chatResponse
	if err := json.Unmarshal(respBody, &cr); err != nil {
		return "", fmt.Errorf("unmarshal response: %w", err)
	}

	if len(cr.Choices) == 0 {
		return "", fmt.Errorf("no choices in response")
	}

	content := cr.Choices[0].Message.Content
	if content == "" {
		// GLM-5.1 and other models may put reasoning in a separate field.
		// Try to extract it from the raw response.
		var raw map[string]interface{}
		if json.Unmarshal(respBody, &raw) == nil {
			if choices, ok := raw["choices"].([]interface{}); ok && len(choices) > 0 {
				if ch, ok := choices[0].(map[string]interface{}); ok {
					if msg, ok := ch["message"].(map[string]interface{}); ok {
						for _, key := range []string{"reasoning", "reasoning_content"} {
							if r, ok := msg[key].(string); ok && r != "" {
								return r, nil
							}
						}
					}
				}
			}
		}
		return "", fmt.Errorf("empty response content")
	}

	return content, nil
}

// Model returns the model name configured for this client.
func (c *Client) Model() string {
	return c.model
}
