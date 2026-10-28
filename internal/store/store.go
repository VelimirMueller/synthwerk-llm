// Package store keeps conversations and messages in Postgres (schema llm). Every
// query runs in a transaction that first sets synthwerk.tenant_id and
// synthwerk.owner_id with SET LOCAL, so row level security limits it to one user in
// one org. The owner comes from the verified token, never from a request body.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/VelimirMueller/synthwerk-contracts/go/kit/cloudevents"
	"github.com/VelimirMueller/synthwerk-contracts/go/kit/outbox"
)

// ErrNotFound means no such conversation for this owner. Another owner's id gives
// the same error as an unknown id.
var ErrNotFound = errors.New("store: not found")

// Owner is the org and the user of the verified token.
type Owner struct {
	Tenant string
	User   string
}

// Message statuses (contract MessageStatus).
const (
	StatusStreaming = "streaming"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCanceled  = "canceled"
)

// Message is one stored message.
type Message struct {
	ID        string
	Role      string
	Content   string
	Status    string
	CreatedAt time.Time
}

// Conversation with its messages, oldest first.
type Conversation struct {
	ID         string
	ModelAlias string
	CreatedAt  time.Time
	Messages   []Message
}

// Turn is one started answer.
type Turn struct {
	UserMessageID      string
	AssistantMessageID string
	// History holds the messages before this turn, oldest first.
	History []Message
}

// Final is the end state of one answer.
type Final struct {
	MessageID string
	Content   string
	Status    string
	TokensIn  int
	TokensOut int
}

// Store is the Postgres store.
type Store struct{ db *pgxpool.Pool }

// New returns the store. The pool's search_path must be llm (kit outbox table).
func New(db *pgxpool.Pool) *Store { return &Store{db: db} }

func (s *Store) withOwner(ctx context.Context, o Owner, fn func(pgx.Tx) error) error {
	if o.Tenant == "" || o.User == "" {
		return errors.New("store: owner is incomplete")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx,
		`SELECT set_config('synthwerk.tenant_id', $1, true), set_config('synthwerk.owner_id', $2, true)`,
		o.Tenant, o.User); err != nil {
		return fmt.Errorf("store: session: %w", err)
	}
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("store: new id: %w", err)
	}
	return id.String(), nil
}

// CreateConversation stores an empty conversation.
func (s *Store) CreateConversation(ctx context.Context, o Owner, alias string) (Conversation, error) {
	id, err := newID()
	if err != nil {
		return Conversation{}, err
	}
	c := Conversation{ID: id, ModelAlias: alias, Messages: []Message{}}
	err = s.withOwner(ctx, o, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO llm.conversation (id, tenant_id, owner_id, model_alias) VALUES ($1, $2, $3, $4) RETURNING created_at`,
			id, o.Tenant, o.User, alias).Scan(&c.CreatedAt)
	})
	if err != nil {
		return Conversation{}, fmt.Errorf("store: create conversation: %w", err)
	}
	return c, nil
}

// ConversationAlias returns the model alias of a conversation, or ErrNotFound.
func (s *Store) ConversationAlias(ctx context.Context, o Owner, id string) (string, error) {
	var alias string
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT model_alias FROM llm.conversation WHERE id = $1 AND tenant_id = $2 AND owner_id = $3 AND deleted_at IS NULL`,
			id, o.Tenant, o.User).Scan(&alias)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("store: conversation: %w", err)
	}
	return alias, nil
}

// GetConversation returns a conversation with its messages, or ErrNotFound.
func (s *Store) GetConversation(ctx context.Context, o Owner, id string) (Conversation, error) {
	c := Conversation{ID: id}
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT model_alias, created_at FROM llm.conversation WHERE id = $1 AND tenant_id = $2 AND owner_id = $3 AND deleted_at IS NULL`,
			id, o.Tenant, o.User).Scan(&c.ModelAlias, &c.CreatedAt); err != nil {
			return err
		}
		var err error
		c.Messages, err = messages(ctx, tx, o, id)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Conversation{}, ErrNotFound
	}
	if err != nil {
		return Conversation{}, fmt.Errorf("store: get conversation: %w", err)
	}
	return c, nil
}

func messages(ctx context.Context, tx pgx.Tx, o Owner, convID string) ([]Message, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, role, content, status, created_at FROM llm.message
		 WHERE conversation_id = $1 AND tenant_id = $2 AND owner_id = $3
		 ORDER BY created_at, id`, convID, o.Tenant, o.User)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Message{}
	for rows.Next() {
		var m Message
		var id uuid.UUID
		if err := rows.Scan(&id, &m.Role, &m.Content, &m.Status, &m.CreatedAt); err != nil {
			return nil, err
		}
		m.ID = id.String()
		out = append(out, m)
	}
	return out, rows.Err()
}

// StartTurn stores the user message and an empty assistant message (streaming) and
// returns the history before them. ErrNotFound when the conversation is not the owner's.
func (s *Store) StartTurn(ctx context.Context, o Owner, convID, text string) (Turn, error) {
	var t Turn
	var err error
	if t.UserMessageID, err = newID(); err != nil {
		return Turn{}, err
	}
	if t.AssistantMessageID, err = newID(); err != nil {
		return Turn{}, err
	}
	err = s.withOwner(ctx, o, func(tx pgx.Tx) error {
		var one int
		if err := tx.QueryRow(ctx,
			`SELECT 1 FROM llm.conversation WHERE id = $1 AND tenant_id = $2 AND owner_id = $3 AND deleted_at IS NULL FOR UPDATE`,
			convID, o.Tenant, o.User).Scan(&one); err != nil {
			return err
		}
		var err error
		if t.History, err = messages(ctx, tx, o, convID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO llm.message (id, conversation_id, tenant_id, owner_id, role, content, status)
			 VALUES ($1, $2, $3, $4, 'user', $5, 'completed')`,
			t.UserMessageID, convID, o.Tenant, o.User, text); err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO llm.message (id, conversation_id, tenant_id, owner_id, role, content, status)
			 VALUES ($1, $2, $3, $4, 'assistant', '', 'streaming')`,
			t.AssistantMessageID, convID, o.Tenant, o.User)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, fmt.Errorf("store: start turn: %w", err)
	}
	return t, nil
}

// Finish stores the end state of the answer and the event in one transaction.
func (s *Store) Finish(ctx context.Context, o Owner, f Final, ev cloudevents.Event) error {
	err := s.withOwner(ctx, o, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`UPDATE llm.message SET content = $2, status = $3, tokens_in = $4, tokens_out = $5
			 WHERE id = $1 AND tenant_id = $6 AND owner_id = $7 AND status = 'streaming'`,
			f.MessageID, f.Content, f.Status, f.TokensIn, f.TokensOut, o.Tenant, o.User)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("message is not streaming")
		}
		return outbox.Enqueue(ctx, tx, ev)
	})
	if err != nil {
		return fmt.Errorf("store: finish: %w", err)
	}
	return nil
}

// SweepStreaming marks answers left in streaming by a crash as failed. Run it at
// startup, before the server takes requests.
func (s *Store) SweepStreaming(ctx context.Context) (int64, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: sweep: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('synthwerk.sweep', 'on', true)`); err != nil {
		return 0, fmt.Errorf("store: sweep: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE llm.message SET status = 'failed' WHERE status = 'streaming'`)
	if err != nil {
		return 0, fmt.Errorf("store: sweep: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("store: sweep: %w", err)
	}
	return tag.RowsAffected(), nil
}
