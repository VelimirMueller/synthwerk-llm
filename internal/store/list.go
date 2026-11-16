package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TitleMax is the length of a list title in characters (contract ConversationSummary).
const TitleMax = 80

// titleScan bounds the text a list query reads of the first user message.
const titleScan = 4 * TitleMax

// Summary is one conversation in the list (synthwerk#54).
type Summary struct {
	ID         string
	ModelAlias string
	Title      string
	CreatedAt  time.Time
}

// ListConversations returns up to limit conversations of the owner in its org, newest
// first (the id is a UUIDv7). before "" starts at the newest; else only ids below it.
// Only conversations with a user message and no deleted_at appear. A widget owner
// (Origin set) sees only the conversations of its own origin. more is true when an
// older conversation exists.
func (s *Store) ListConversations(ctx context.Context, o Owner, before string, limit int) (items []Summary, more bool, err error) {
	if limit < 1 {
		return nil, false, fmt.Errorf("store: list limit %d", limit)
	}
	var cursor any
	if before != "" {
		id, err := uuid.Parse(before)
		if err != nil {
			return nil, false, fmt.Errorf("store: list cursor: %w", err)
		}
		cursor = id
	}
	items = []Summary{}
	err = s.withOwner(ctx, o, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT c.id, c.model_alias, c.created_at, left(m.content, $6)
			 FROM llm.conversation c
			 CROSS JOIN LATERAL (
			     SELECT content FROM llm.message
			     WHERE conversation_id = c.id AND tenant_id = $2 AND owner_id = $3 AND role = 'user'
			     ORDER BY created_at, id LIMIT 1
			 ) m
			 WHERE c.tenant_id = $2 AND c.owner_id = $3 AND c.deleted_at IS NULL
			   AND ($4 = '' OR c.origin = $4)
			   AND ($1::uuid IS NULL OR c.id < $1::uuid)
			 ORDER BY c.id DESC
			 LIMIT $5`,
			cursor, o.Tenant, o.User, o.Origin, limit+1, titleScan)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sm Summary
			var id uuid.UUID
			var first string
			if err := rows.Scan(&id, &sm.ModelAlias, &sm.CreatedAt, &first); err != nil {
				return err
			}
			sm.ID = id.String()
			sm.Title = Title(first)
			items = append(items, sm)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, fmt.Errorf("store: list conversations: %w", err)
	}
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
}

// Title makes a list title of the first user message: white space joined to one
// space, at most TitleMax characters, invalid UTF-8 replaced. A message of white
// space only gives "…": the contract title is never empty.
func Title(text string) string {
	t := strings.Join(strings.Fields(strings.ToValidUTF8(text, "�")), " ")
	if utf8.RuneCountInString(t) > TitleMax {
		r := []rune(t)
		t = strings.TrimRight(string(r[:TitleMax-1]), " ") + "…"
	}
	if t == "" {
		return "…"
	}
	return t
}
