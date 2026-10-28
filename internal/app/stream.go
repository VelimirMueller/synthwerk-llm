package app

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// StreamName is the JetStream stream of the llm facts. The llm service owns it.
const StreamName = "EVT_LLM"

// EnsureStream creates or updates EVT_LLM (subjects evt.llm.>).
func EnsureStream(ctx context.Context, js jetstream.JetStream) error {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:        StreamName,
		Description: "Facts of synthwerk-llm (CloudEvents, metadata only).",
		Subjects:    []string{"evt.llm.>"},
		Storage:     jetstream.FileStorage,
		MaxAge:      30 * 24 * time.Hour,
		Duplicates:  2 * time.Minute, // >= relay restart time, see kit/outbox
	})
	if err != nil {
		return fmt.Errorf("app: stream %s: %w", StreamName, err)
	}
	return nil
}
