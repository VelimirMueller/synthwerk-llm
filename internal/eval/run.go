package eval

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/VelimirMueller/synthwerk-llm/internal/provider"
)

// Options of a run. Every field is fixed so two runs of the same model give the
// same answers.
type Options struct {
	// Alias is the model alias under test, Model the provider model it maps to.
	Alias, Model string
	// SystemPrompt is the gateway system prompt (cases may replace it).
	SystemPrompt string
	// Seed of the sampler. Temperature is always 0.
	Seed int
	// MaxTokens of a case without its own value.
	MaxTokens int
	// CaseTimeout bounds one case (model load included).
	CaseTimeout time.Duration
}

// CaseResult is the outcome of one case. It holds the answer for the report, which
// is fine here: every prompt is synthetic and public (no user data in an eval set).
type CaseResult struct {
	ID       string   `json:"id"`
	Category string   `json:"category"`
	Critical bool     `json:"critical"`
	Passed   bool     `json:"passed"`
	Failures []string `json:"failures,omitempty"`
	Error    string   `json:"error,omitempty"`
	Answer   string   `json:"answer"`
	// Speed, report only.
	TTFTMillis   int64  `json:"ttft_ms"`
	TotalMillis  int64  `json:"total_ms"`
	OutputTokens int    `json:"output_tokens"`
	Finish       string `json:"finish,omitempty"`
}

// think matches a thinking trace that a model put into the content anyway.
var think = regexp.MustCompile(`(?s)<think>.*?</think>`)

// Run sends every case to the provider, one after the other, and checks the answers.
// A provider error fails the case; Run itself only fails when ctx ends.
func Run(ctx context.Context, p provider.Provider, cases []Case, o Options) ([]CaseResult, error) {
	if o.CaseTimeout <= 0 {
		o.CaseTimeout = 3 * time.Minute
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = 128
	}
	out := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		out = append(out, runCase(ctx, p, c, o))
	}
	return out, nil
}

func runCase(ctx context.Context, p provider.Provider, c Case, o Options) CaseResult {
	r := CaseResult{ID: c.ID, Category: c.Category, Critical: c.Critical}
	ctx, cancel := context.WithTimeout(ctx, o.CaseTimeout)
	defer cancel()

	req := request(c, o)
	start := time.Now()
	st, err := p.Chat(ctx, req)
	if err != nil {
		r.Error = "chat: " + err.Error()
		return r
	}
	defer func() { _ = st.Close() }()
	var b strings.Builder
	for {
		piece, err := st.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.Error = "stream: " + err.Error()
			r.Answer = b.String()
			return r
		}
		if b.Len() == 0 && piece != "" {
			r.TTFTMillis = time.Since(start).Milliseconds()
		}
		b.WriteString(piece)
	}
	r.TotalMillis = time.Since(start).Milliseconds()
	res := st.Result()
	r.OutputTokens = res.Usage.OutputTokens
	r.Finish = string(res.Finish)
	r.Answer = strings.TrimSpace(think.ReplaceAllString(b.String(), ""))

	r.Passed = true
	for i := range c.Checks {
		if ok, why := c.Checks[i].Apply(r.Answer); !ok {
			r.Passed = false
			r.Failures = append(r.Failures, c.Checks[i].Type+": "+why)
		}
	}
	return r
}

// request builds the model call of a case the way the gateway does: system prompt
// first, then the turns.
func request(c Case, o Options) provider.Request {
	temp, seed := 0.0, o.Seed
	maxTokens := c.MaxTokens
	if maxTokens == 0 {
		maxTokens = o.MaxTokens
	}
	sys := o.SystemPrompt
	if c.System != "" {
		sys = c.System
	}
	msgs := make([]provider.Message, 0, len(c.Messages)+1)
	if sys != "" {
		msgs = append(msgs, provider.Message{Role: provider.RoleSystem, Text: sys})
	}
	for _, m := range c.Messages {
		msgs = append(msgs, provider.Message{Role: provider.Role(m.Role), Text: m.Content})
	}
	return provider.Request{
		Model:    o.Model,
		Messages: msgs,
		Options:  provider.Options{Temperature: &temp, Seed: &seed, MaxTokens: maxTokens, NoThinking: true},
	}
}
