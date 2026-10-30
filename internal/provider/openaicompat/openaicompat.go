// Package openaicompat calls an OpenAI-compatible chat completions endpoint with
// streaming, for example Ollama at http://ollama:11434/v1. It uses its own thin HTTP
// client, no vendor SDK. Error values never hold the response body or the prompt.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
)

// Config of the adapter.
type Config struct {
	// Name for logs and events: ollama or openai_compat.
	Name string
	// BaseURL ends before /chat/completions, for example http://ollama:11434/v1.
	BaseURL string
	// APIKey is sent as a Bearer token when set. Never logged.
	APIKey string
	// HeaderTimeout is the max wait for the response headers. Ollama loads the model
	// before it answers, so this is long. Default 120 s.
	HeaderTimeout time.Duration
	// Client overrides the HTTP client (tests).
	Client *http.Client
}

// Provider is the adapter.
type Provider struct {
	name   string
	url    string
	key    string
	client *http.Client
}

// New checks the config and returns the adapter.
func New(cfg Config) (*Provider, error) {
	if cfg.Name == "" {
		cfg.Name = "openai_compat"
	}
	if !strings.HasPrefix(cfg.BaseURL, "http://") && !strings.HasPrefix(cfg.BaseURL, "https://") {
		return nil, errors.New("openaicompat: base URL must start with http:// or https://")
	}
	if cfg.HeaderTimeout <= 0 {
		cfg.HeaderTimeout = 120 * time.Second
	}
	client := cfg.Client
	if client == nil {
		// No Client.Timeout: it would cut a long stream. The request context bounds it.
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			ResponseHeaderTimeout: cfg.HeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   4,
			ForceAttemptHTTP2:     true,
		}}
	}
	return &Provider{
		name:   cfg.Name,
		url:    strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		key:    cfg.APIKey,
		client: client,
	}, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return p.name }

type wireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type wireRequest struct {
	Model         string        `json:"model"`
	Messages      []wireMessage `json:"messages"`
	Stream        bool          `json:"stream"`
	StreamOptions struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Temperature *float64 `json:"temperature,omitempty"`
	Seed        *int     `json:"seed,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
	// ReasoningEffort "none" turns the thinking trace off on Ollama (qwen3.5). Checked
	// with Ollama: "think": false is ignored on the OpenAI endpoint.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type wireChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Chat implements provider.Provider.
func (p *Provider) Chat(ctx context.Context, req provider.Request) (provider.Stream, error) {
	body := wireRequest{Model: req.Model, Stream: true, Messages: make([]wireMessage, len(req.Messages))}
	body.StreamOptions.IncludeUsage = true
	body.Temperature, body.Seed, body.MaxTokens = req.Options.Temperature, req.Options.Seed, req.Options.MaxTokens
	if req.Options.NoThinking {
		body.ReasoningEffort = "none"
	}
	for i, m := range req.Messages {
		body.Messages[i] = wireMessage{Role: string(m.Role), Content: m.Text}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: encode request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("openaicompat: new request: %w", err)
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	if p.key != "" {
		hreq.Header.Set("Authorization", "Bearer "+p.key)
	}
	res, err := p.client.Do(hreq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The error names no URL, no network cause and no body.
		return nil, provider.ErrConnect
	}
	if res.StatusCode != http.StatusOK {
		// Drain a little so the connection can be reused. The body is never kept.
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
		_ = res.Body.Close()
		return nil, &provider.StatusError{Code: res.StatusCode}
	}
	sc := bufio.NewScanner(res.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	return &stream{body: res.Body, sc: sc, input: provider.EstimateInput(req.Messages)}, nil
}

// ErrStream means the provider sent a stream the adapter cannot read. It is
// provider.ErrStream, so the fallback chain sees it without this package.
var ErrStream = provider.ErrStream

type stream struct {
	body     io.ReadCloser
	sc       *bufio.Scanner
	pending  []string
	done     bool
	input    int
	outChars strings.Builder
	usage    *provider.Usage
	finish   provider.FinishReason
	closed   bool
}

func (s *stream) Next() (string, error) {
	for {
		if len(s.pending) > 0 {
			piece := s.pending[0]
			s.pending = s.pending[1:]
			return piece, nil
		}
		if s.done {
			return "", io.EOF
		}
		if !s.sc.Scan() {
			if err := s.sc.Err(); err != nil {
				return "", fmt.Errorf("%w: read: %w", ErrStream, cause(err))
			}
			// The body ended without [DONE]: treat as the end only after a finish reason.
			if s.finish == "" {
				return "", fmt.Errorf("%w: stream ended early", ErrStream)
			}
			s.done = true
			continue
		}
		line := s.sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue // comments, event names, blank lines
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			s.done = true
			continue
		}
		var c wireChunk
		if err := json.Unmarshal([]byte(data), &c); err != nil {
			return "", fmt.Errorf("%w: chunk is not JSON", ErrStream)
		}
		if c.Usage != nil {
			s.usage = &provider.Usage{InputTokens: c.Usage.PromptTokens, OutputTokens: c.Usage.CompletionTokens}
		}
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				s.pending = append(s.pending, ch.Delta.Content)
				s.outChars.WriteString(ch.Delta.Content)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				s.finish = provider.FinishStop
				if *ch.FinishReason == "length" {
					s.finish = provider.FinishLength
				}
			}
		}
	}
}

// cause keeps the context errors and hides the rest behind a fixed text.
func cause(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("connection error")
}

func (s *stream) Result() provider.Result {
	r := provider.Result{Finish: s.finish}
	if r.Finish == "" {
		r.Finish = provider.FinishStop
	}
	if s.usage != nil {
		r.Usage = *s.usage
	} else {
		r.Usage = provider.Usage{InputTokens: s.input, OutputTokens: provider.EstimateTokens(s.outChars.String()), Estimated: true}
	}
	return r
}

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.body.Close()
}
