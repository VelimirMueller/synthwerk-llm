// Package provider is the model layer of the gateway. An adapter turns one chat
// request into a stream of text pieces. The interface stays small on purpose: the
// chat handler, the eval runner (#72) and every adapter (fake, openaicompat, later
// anthropic) share it.
//
// Rules for an adapter:
//   - Errors never carry the provider's response body or the prompt text.
//   - Chat returns ErrUnavailable (wrapped) when the model cannot answer before the
//     first piece. Errors from Next after the first piece end the answer as failed.
//   - The context of Chat bounds the whole stream: cancel it to stop the model.
package provider

import (
	"context"
	"errors"
	"unicode/utf8"
)

// Role of a message.
type Role string

// Roles the gateway sends to a model.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one turn of the conversation, in the one internal shape.
type Message struct {
	Role Role
	Text string
}

// Request is one chat call.
type Request struct {
	// Model is the provider model name, for example qwen3.5:4b. Never an alias.
	Model string
	// Messages, oldest first. The system prompt, if any, is the first one.
	Messages []Message
	// Options tune the sampling. The zero value sends nothing: the model defaults apply.
	Options Options
}

// Options tune one call. The eval runner (#72) pins them for repeatable answers.
type Options struct {
	// Temperature, nil: the model default.
	Temperature *float64
	// Seed of the sampler, nil: random.
	Seed *int
	// MaxTokens bounds the answer, 0: no bound.
	MaxTokens int
	// NoThinking asks a reasoning model (qwen3.5) to answer without a thinking trace.
	NoThinking bool
}

// FinishReason says why the model stopped.
type FinishReason string

// Finish reasons, the same values as the contract's done event.
const (
	FinishStop   FinishReason = "stop"
	FinishLength FinishReason = "length"
)

// Usage is the token use of one answer.
type Usage struct {
	InputTokens  int
	OutputTokens int
	// Estimated is true when the provider sent no numbers and the adapter counted.
	Estimated bool
}

// Result is valid after Next returned io.EOF.
type Result struct {
	Usage  Usage
	Finish FinishReason
}

// Stream is one answer in pieces. It is not safe for concurrent use.
type Stream interface {
	// Next returns the next piece of text, or io.EOF after the last one.
	Next() (string, error)
	// Result returns the usage and the finish reason. Call it after io.EOF.
	Result() Result
	// Close releases the connection. It is safe to call more than once.
	Close() error
}

// Provider is one model backend.
type Provider interface {
	// Name is the adapter name for logs and events, for example ollama or fake.
	Name() string
	// Chat starts one answer.
	Chat(ctx context.Context, req Request) (Stream, error)
}

// ErrUnavailable means the model cannot answer now (down, busy, bad status).
var ErrUnavailable = errors.New("provider: unavailable")

// EstimateTokens is a rough token count for providers that send no usage:
// about four characters per token, at least one for non-empty text.
func EstimateTokens(text string) int {
	n := utf8.RuneCountInString(text)
	if n == 0 {
		return 0
	}
	return max(1, n/4)
}

// EstimateInput estimates the input tokens of a request.
func EstimateInput(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateTokens(m.Text)
	}
	return total
}
