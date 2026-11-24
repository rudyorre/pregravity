package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Content represents a Gemini conversation turn.
type Content struct {
	Role  string `json:"role,omitempty"` // "user", "model"
	Parts []Part `json:"parts"`
}

// Part represents a single piece of content inside a turn.
type Part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

// FunctionCall represents a tool call emitted by Gemini.
type FunctionCall struct {
	Name string         `json:"name"`
	Args map[string]any `json:"args,omitempty"`
}

// FunctionResponse represents the structured result returned to Gemini.
type FunctionResponse struct {
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

// Tool declares a set of callable functions for Gemini.
type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations"`
}

// FunctionDeclaration describes a single tool schema.
type FunctionDeclaration struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// GenerateRequest is the payload sent to models/{model}:generateContent.
type GenerateRequest struct {
	SystemInstruction *Content  `json:"systemInstruction,omitempty"`
	Contents          []Content `json:"contents"`
	Tools             []Tool    `json:"tools,omitempty"`
}

// GenerateResponse is the subset of Gemini's response we consume.
type GenerateResponse struct {
	Candidates []struct {
		Content      Content `json:"content"`
		FinishReason string  `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *UsageMetadata `json:"usageMetadata,omitempty"`
	Error         *APIError      `json:"error,omitempty"`
	ServiceTier   string         `json:"-"`
}

// UsageMetadata records token counts reported by the Gemini API.
type UsageMetadata struct {
	PromptTokenCount     int `json:"promptTokenCount"`
	CandidatesTokenCount int `json:"candidatesTokenCount"`
	TotalTokenCount      int `json:"totalTokenCount"`
}

// APIError is an error payload returned by the Gemini REST API.
type APIError struct {
	Code    int              `json:"code"`
	Message string           `json:"message"`
	Status  string           `json:"status"`
	Details []map[string]any `json:"details,omitempty"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("gemini api error (%d %s): %s", e.Code, e.Status, e.Message)
}

// QuotaLimit extracts a numeric daily quotaValue if Google returned a QuotaFailure detail on 429.
func (e *APIError) QuotaLimit() int {
	if e == nil {
		return 0
	}
	for _, d := range e.Details {
		violations, ok := d["violations"].([]any)
		if !ok {
			continue
		}
		for _, v := range violations {
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			qid, _ := m["quotaId"].(string)
			if qid != "" && !strings.Contains(strings.ToLower(qid), "day") {
				continue
			}
			if rawVal, ok := m["quotaValue"].(string); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(rawVal)); err == nil && n > 0 {
					return n
				}
			}
		}
	}
	return 0
}

// Provider is the minimal interface used by Assistant (easy to mock in tests).
type Provider interface {
	Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error)
}

// Client is a pure-stdlib REST client for the Google Gemini Developer API.
type Client struct {
	apiKey     string
	model      string
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a Gemini API client.
func NewClient(apiKey, model, baseURL string, timeout time.Duration) *Client {
	if baseURL == "" {
		baseURL = "https://generativelanguage.googleapis.com/v1beta"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &Client{
		apiKey:  strings.TrimSpace(apiKey),
		model:   strings.TrimSpace(model),
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

// Generate sends a single generateContent request using x-goog-api-key header auth
// (avoids putting API keys in query strings / proxy logs).
func (c *Client) Generate(ctx context.Context, req GenerateRequest) (*GenerateResponse, error) {
	if c.apiKey == "" {
		return nil, errors.New("GEMINI_API_KEY is not set (add it to ~/.config/pregravity/env or export GEMINI_API_KEY)")
	}
	if c.model == "" {
		return nil, errors.New("gemini model is not configured")
	}

	endpoint := fmt.Sprintf("%s/models/%s:generateContent", c.baseURL, url.PathEscape(c.model))
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(attempt) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}

		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("x-goog-api-key", c.apiKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			lastErr = fmt.Errorf("call gemini api: %w", err)
			continue
		}

		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = fmt.Errorf("read gemini response: %w", readErr)
			continue
		}

		var parsed GenerateResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("decode gemini response (HTTP %d): %w", resp.StatusCode, err)
				continue
			}
			return nil, fmt.Errorf("decode gemini response (HTTP %d): %w", resp.StatusCode, err)
		}
		if parsed.Error != nil {
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				lastErr = parsed.Error
				continue
			}
			return nil, parsed.Error
		}
		if resp.StatusCode >= 400 {
			err := fmt.Errorf("gemini api returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
			if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
				lastErr = err
				continue
			}
			return nil, err
		}
		if len(parsed.Candidates) == 0 {
			return nil, errors.New("gemini api returned no candidates")
		}
		parsed.ServiceTier = strings.TrimSpace(resp.Header.Get("x-gemini-service-tier"))
		return &parsed, nil
	}
	return nil, lastErr
}
