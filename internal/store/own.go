package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxExportItems is the contract maximum (LlmExport) of conversations, of quota
// rows, and of messages in one conversation.
const MaxExportItems = 100000

// ErrExportTooLarge means the user has more rows than the export contract allows.
var ErrExportTooLarge = errors.New("store: export is larger than the contract allows")

// idRe is the shape of a token sub and of a Zitadel org id. It also keeps a comma
// out of synthwerk.erase_orgs.
var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ExportConversation is one conversation of the export, without its messages.
type ExportConversation struct {
	ID         string
	Tenant     string
	ModelAlias string
	CreatedAt  time.Time
}

// ExportMessage is one message of the export.
type ExportMessage struct {
	Message
	// TokensIn and TokensOut are nil when the answer did not finish.
	TokensIn  *int32
	TokensOut *int32
}

// ExportQuota is one quota row of the user.
type ExportQuota struct {
	Tenant      string
	WindowStart time.Time
	Used        int64
}

// ExportSink takes the rows of an export in order: each conversation, then its
// messages, then all quota rows. An error stops the export.
type ExportSink interface {
	Conversation(c ExportConversation) error
	Message(m ExportMessage) error
	Quota(q ExportQuota) error
}

// Export reads every conversation (also one marked deleted), message and own quota
// row of user in every org, oldest first, and hands them to sink row by row. One
// READ ONLY, REPEATABLE READ transaction: one snapshot, nothing written. Policy
// export_read (migration 0004) opens the user's rows of every org to it, never the
// org quota rows. ErrExportTooLarge before the first row when a list passes the
// contract maximum.
func (s *Store) Export(ctx context.Context, user string, sink ExportSink) error {
	if !idRe.MatchString(user) {
		return errors.New("store: export: user is not a valid sub")
	}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return fmt.Errorf("store: export: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('synthwerk.export_owner', $1, true)`, user); err != nil {
		return fmt.Errorf("store: export: session: %w", err)
	}
	var convs, quotas, msgs int64
	if err := tx.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM llm.conversation WHERE owner_id = $1),
		       (SELECT count(*) FROM llm.quota_usage WHERE owner_id = $1),
		       (SELECT coalesce(max(n), 0) FROM (SELECT count(*) AS n FROM llm.message WHERE owner_id = $1 GROUP BY conversation_id) m)`,
		user).Scan(&convs, &quotas, &msgs); err != nil {
		return fmt.Errorf("store: export: count: %w", err)
	}
	if convs > MaxExportItems || quotas > MaxExportItems || msgs > MaxExportItems {
		return ErrExportTooLarge
	}
	if err := exportConversations(ctx, tx, user, sink); err != nil {
		return err
	}
	if err := exportQuota(ctx, tx, user, sink); err != nil {
		return err
	}
	// Read only: the commit only ends the snapshot.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: export: commit: %w", err)
	}
	return nil
}

// exportConversations streams conversations and messages in one ordered join. pgx
// reads the rows from the network one by one, so memory stays one row.
func exportConversations(ctx context.Context, tx pgx.Tx, user string, sink ExportSink) error {
	rows, err := tx.Query(ctx, `
		SELECT c.id, c.tenant_id, c.model_alias, c.created_at,
		       m.id, m.role, m.content, m.status, m.tokens_in, m.tokens_out, m.created_at
		FROM llm.conversation c
		LEFT JOIN llm.message m ON m.conversation_id = c.id AND m.owner_id = c.owner_id
		WHERE c.owner_id = $1
		ORDER BY c.created_at, c.id, m.created_at, m.id`, user)
	if err != nil {
		return fmt.Errorf("store: export: conversations: %w", err)
	}
	defer rows.Close()
	var last uuid.UUID
	for rows.Next() {
		var c ExportConversation
		var cid uuid.UUID
		var mid *uuid.UUID
		var role, content, status *string
		var at *time.Time
		var m ExportMessage
		if err := rows.Scan(&cid, &c.Tenant, &c.ModelAlias, &c.CreatedAt,
			&mid, &role, &content, &status, &m.TokensIn, &m.TokensOut, &at); err != nil {
			return fmt.Errorf("store: export: conversations: %w", err)
		}
		if cid != last {
			last = cid
			c.ID = cid.String()
			if err := sink.Conversation(c); err != nil {
				return err
			}
		}
		if mid == nil { // conversation without messages
			continue
		}
		m.ID, m.Role, m.Content, m.Status, m.CreatedAt = mid.String(), deref(role), deref(content), deref(status), derefTime(at)
		if err := sink.Message(m); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: export: conversations: %w", err)
	}
	return nil
}

func exportQuota(ctx context.Context, tx pgx.Tx, user string, sink ExportSink) error {
	rows, err := tx.Query(ctx, `
		SELECT tenant_id, window_start, used FROM llm.quota_usage
		WHERE owner_id = $1 ORDER BY window_start, tenant_id`, user)
	if err != nil {
		return fmt.Errorf("store: export: quota: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var q ExportQuota
		if err := rows.Scan(&q.Tenant, &q.WindowStart, &q.Used); err != nil {
			return fmt.Errorf("store: export: quota: %w", err)
		}
		if err := sink.Quota(q); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: export: quota: %w", err)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Erased counts the rows one erase deleted.
type Erased struct {
	Messages      int64
	Conversations int64
	QuotaRows     int64
}

// EraseUser deletes every message, conversation and own quota row of user in every
// org, and the org quota rows (owner_id = ”) of orgs, in one transaction. Policy
// erase (migration 0004) allows exactly these rows. A repeat deletes nothing and
// returns no error.
func (s *Store) EraseUser(ctx context.Context, user string, orgs []string) (Erased, error) {
	if !idRe.MatchString(user) {
		return Erased{}, errors.New("store: erase: user is not a valid sub")
	}
	for _, o := range orgs {
		if !idRe.MatchString(o) {
			return Erased{}, errors.New("store: erase: org is not a valid org id")
		}
	}
	var e Erased
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return Erased{}, fmt.Errorf("store: erase: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx,
		`SELECT set_config('synthwerk.erase_owner', $1, true), set_config('synthwerk.erase_orgs', $2, true)`,
		user, strings.Join(orgs, ",")); err != nil {
		return Erased{}, fmt.Errorf("store: erase: session: %w", err)
	}
	if orgs == nil {
		orgs = []string{} // ANY(NULL) would match nothing anyway; keep the type plain
	}
	// Messages first: they reference the conversation.
	steps := []struct {
		n    *int64
		sql  string
		args []any
	}{
		{&e.Messages, `DELETE FROM llm.message WHERE owner_id = $1`, []any{user}},
		{&e.Conversations, `DELETE FROM llm.conversation WHERE owner_id = $1`, []any{user}},
		{&e.QuotaRows, `DELETE FROM llm.quota_usage WHERE owner_id = $1 OR (owner_id = '' AND tenant_id = ANY($2::text[]))`, []any{user, orgs}},
	}
	for _, st := range steps {
		tag, err := tx.Exec(ctx, st.sql, st.args...)
		if err != nil {
			return Erased{}, fmt.Errorf("store: erase: %w", err)
		}
		*st.n = tag.RowsAffected()
	}
	if err := tx.Commit(ctx); err != nil {
		return Erased{}, fmt.Errorf("store: erase: commit: %w", err)
	}
	return e, nil
}
