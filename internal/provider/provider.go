// Package provider is the model layer of the gateway. An adapter turns one chat
// request into a stream of text pieces. The interface stays small on purpose: the
// chat handler, the eval runner (#72) and every adapter (fake, openaicompat, later
// anthropic) share it.
//
// Rules for an adapter:
//   - Errors never carry the provider's response body or the prompt text.
//   - Chat returns ErrUnavailable (wrapped) when the model cannot answer before the
//     first piece: ErrConnect when the provider is not reachable, a *StatusError for
//     a bad HTTP status. Errors from Next after the first piece end the answer as
//     failed. A stream the adapter cannot read is ErrStream (wrapped).
//   - The context of Chat bounds the whole stream: cancel it to stop the model.
package provider

import (
	"context"
	"errors"
	"fmt"
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
	// Alias is the model alias the gateway resolved Model from. Adapters ignore it;
	// the fallback chain (internal/provider/chain, #56) picks each provider's model
	// by it. Empty: Model goes to the first provider only.
	Alias string
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

// ErrConnect means the provider was not reachable: no connection, no response
// headers. It matches ErrUnavailable.
var ErrConnect = fmt.Errorf("%w: request failed", ErrUnavailable)

// ErrStream means the provider sent a stream the adapter cannot read.
var ErrStream = errors.New("provider: bad stream")

// StatusError is a bad HTTP status of the provider. It matches ErrUnavailable. It
// never holds the response body.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string { return fmt.Sprintf("%s: status %d", ErrUnavailable, e.Code) }

// Is makes errors.Is(err, ErrUnavailable) true.
func (*StatusError) Is(target error) bool { return target == ErrUnavailable }

// Answered is implemented by a stream that knows which provider answered (the
// fallback chain). name is the provider's name; taken is false when no provider
// took the request.
type Answered interface {
	Answered() (name string, taken bool)
}

// AnsweredBy returns the provider that answered st: Answered when st implements
// it, else def (taken true).
func AnsweredBy(st Stream, def string) (name string, taken bool) {
	if a, ok := st.(Answered); ok {
		return a.Answered()
	}
	return def, true
}

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
