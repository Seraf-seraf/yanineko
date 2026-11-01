package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"database/sql/driver"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	turso "turso.tech/database/tursogo-serverless"

	"github.com/Seraf-seraf/yanineko/internal/app"
	"github.com/Seraf-seraf/yanineko/internal/domain"
)

const cleanupBatchSize = 100
const earlierPendingScanLimit = 100
const processedUpdateRetention = 90 * 24 * time.Hour
const microsecondsPerDay = 24 * 60 * 60 * 1_000_000

var ErrDatabaseConfiguration = errors.New("invalid SQLite configuration")
var ErrDatabaseUnavailable = errors.New("SQLite database unavailable")
var ErrDatabaseOperation = errors.New("SQLite database operation failed")
var ErrSchemaMigration = errors.New("SQLite schema migration failed")
var ErrLeaseLost = errors.New("conversation lease lost")
var ErrUpdateConflict = errors.New("Telegram update id conflict")
var ErrUpdateNotFound = errors.New("Telegram update not found")

//go:embed migrations/*.sql
var migrationFS embed.FS

type Store struct {
	db *sql.DB
}

type transaction struct {
	conn *sql.Conn
	done bool
}

type conversationRecord struct {
	Version    int64
	LeaseOwner sql.NullString
	LeaseUntil sql.NullFloat64
	Now        float64
}

type updateRecord struct {
	UpdateID           int64
	ChatID             int64
	ThreadID           int64
	SourceMessageID    int64
	InputText          string
	Kind               string
	State              string
	ProcessingOwner    string
	ProcessingVersion  int64
	ResponseText       sql.NullString
	TelegramMessageID  sql.NullInt64
	DeliveryOwner      sql.NullString
	DeliveryLeaseUntil sql.NullFloat64
}

func Open(ctx context.Context, databaseURL string, authToken string) (*Store, error) {
	if !validDatabaseURL(databaseURL) || !validToken(authToken) {
		return nil, ErrDatabaseConfiguration
	}
	db := sql.OpenDB(turso.NewConnector(databaseURL, authToken))
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(time.Minute)
	db.SetConnMaxIdleTime(10 * time.Second)
	store := newStore(db)
	pingContext, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if err := db.PingContext(pingContext); err != nil {
		db.Close()
		return nil, databaseError(pingContext, err, ErrDatabaseUnavailable)
	}
	if err := store.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func newStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) begin(ctx context.Context) (*transaction, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		_ = conn.Close()
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return &transaction{conn: conn}, nil
}

func (tx *transaction) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx.done {
		return nil, ErrDatabaseOperation
	}
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx *transaction) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if tx.done {
		return nil, ErrDatabaseOperation
	}
	return tx.conn.QueryContext(ctx, query, args...)
}

func (tx *transaction) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if tx.done {
		return tx.conn.QueryRowContext(ctx, "SELECT 1 WHERE 0")
	}
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (tx *transaction) Commit(ctx context.Context) error {
	if tx.done {
		return ErrDatabaseOperation
	}
	tx.done = true
	_, err := tx.conn.ExecContext(ctx, "COMMIT")
	if err != nil {
		_ = tx.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	return errors.Join(err, tx.conn.Close())
}

func (tx *transaction) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := tx.conn.ExecContext(ctx, "ROLLBACK")
	if err != nil {
		_ = tx.conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	return errors.Join(err, tx.conn.Close())
}

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS yanineko_schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at REAL NOT NULL DEFAULT (julianday('now'))
		)`); err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	for version, name := range []string{"migrations/001_initial.sql", "migrations/002_model_contexts.sql", "migrations/003_archive_order.sql"} {
		migrationVersion := version + 1
		var applied int64
		if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN EXISTS (SELECT 1 FROM yanineko_schema_migrations WHERE version = ?) THEN 1 ELSE 0 END`, migrationVersion).Scan(&applied); err != nil {
			return databaseError(ctx, err, ErrSchemaMigration)
		}
		if applied != 0 {
			continue
		}
		script, err := migrationFS.ReadFile(name)
		if err != nil {
			return ErrSchemaMigration
		}
		for _, statement := range strings.Split(string(script), ";") {
			statement = strings.TrimSpace(statement)
			if statement == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return databaseError(ctx, err, ErrSchemaMigration)
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO yanineko_schema_migrations (version) VALUES (?)`, migrationVersion); err != nil {
			return databaseError(ctx, err, ErrSchemaMigration)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrSchemaMigration)
	}
	return nil
}

func (s *Store) Begin(ctx context.Context, incoming domain.Incoming, leaseDuration time.Duration) (app.BeginResult, error) {
	if !validIncoming(incoming) || !validateDuration(leaseDuration) {
		return app.BeginResult{}, ErrDatabaseConfiguration
	}
	existingKey, exists, err := s.findUpdateKey(ctx, incoming.UpdateID)
	if err != nil {
		return app.BeginResult{}, err
	}
	if exists && existingKey != incoming.Key {
		return app.BeginResult{}, ErrUpdateConflict
	}
	owner, err := ownerToken()
	if err != nil {
		return app.BeginResult{}, ErrDatabaseOperation
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	if err := cleanupStale(ctx, tx); err != nil {
		return app.BeginResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversations (chat_id, thread_id)
		VALUES (?, ?)
		ON CONFLICT (chat_id, thread_id) DO NOTHING`, incoming.Key.ChatID, incoming.Key.ThreadID); err != nil {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	conversation, err := loadConversation(ctx, tx, incoming.Key)
	if err != nil {
		return app.BeginResult{}, err
	}
	existing, err := loadUpdate(ctx, tx, incoming.UpdateID)
	if err == nil {
		if !matchesIncoming(existing, incoming) {
			return app.BeginResult{}, ErrUpdateConflict
		}
		result, err := s.beginExisting(ctx, tx, conversation, existing, owner, leaseDuration)
		if err != nil {
			return app.BeginResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}

	for scanned := 0; scanned < earlierPendingScanLimit; scanned++ {
		pending, pendingErr := loadPending(ctx, tx, incoming.Key, incoming.UpdateID)
		if errors.Is(pendingErr, sql.ErrNoRows) {
			break
		}
		if pendingErr != nil {
			return app.BeginResult{}, databaseError(ctx, pendingErr, ErrDatabaseOperation)
		}
		result, done, pendingErr := s.beginEarlierPending(ctx, tx, &conversation, pending, owner, leaseDuration)
		if pendingErr != nil {
			return app.BeginResult{}, pendingErr
		}
		if done {
			if err := tx.Commit(ctx); err != nil {
				return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
			}
			return result, nil
		}
	}
	remaining, err := loadPending(ctx, tx, incoming.Key, incoming.UpdateID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if err == nil {
		if err := tx.Commit(ctx); err != nil {
			return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		return app.BeginResult{Status: app.BeginBusy, Claim: claimFromRecord(remaining)}, nil
	}
	if leaseActive(conversation) {
		return app.BeginResult{Status: app.BeginBusy}, nil
	}
	version := conversation.Version + 1
	if err := setConversationLease(ctx, tx, incoming.Key, version, owner, leaseDuration); err != nil {
		return app.BeginResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO processed_updates (
			update_id, chat_id, thread_id, source_message_id, input_text, kind, state,
			processing_owner, processing_version
		) VALUES (?, ?, ?, ?, ?, ?, 'processing', ?, ?)`,
		incoming.UpdateID, incoming.Key.ChatID, incoming.Key.ThreadID, incoming.MessageID,
		incoming.Text, kindName(incoming.Kind), owner, version); err != nil {
		if isConstraintError(err) {
			return app.BeginResult{}, ErrUpdateConflict
		}
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if incoming.Kind == domain.MessageText {
		if err := insertArchivedUserMessage(ctx, tx, incoming.Key, incoming.UpdateID, incoming.Text); err != nil {
			return app.BeginResult{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return app.BeginResult{Status: app.BeginClaimed, Claim: claimFromIncoming(incoming, owner, version)}, nil
}

func (s *Store) History(ctx context.Context, claim app.Claim, afterUpdateID int64) ([]domain.Message, error) {
	if !validClaim(claim) || claim.Incoming.Kind != domain.MessageText {
		return nil, ErrLeaseLost
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	conversation, err := loadConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return nil, err
	}
	update, err := loadUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return nil, safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return nil, ErrLeaseLost
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT update_id, archive_order, role, content
		FROM conversation_messages
		WHERE chat_id = ? AND thread_id = ? AND archive_order > ?
		ORDER BY archive_order`,
		claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, afterUpdateID)
	if err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer rows.Close()
	history := make([]domain.Message, 0)
	for rows.Next() {
		var message domain.Message
		if err := rows.Scan(&message.UpdateID, &message.ArchiveOrder, &message.Role, &message.Text); err != nil {
			return nil, databaseError(ctx, err, ErrDatabaseOperation)
		}
		history = append(history, message)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return history, nil
}

func (s *Store) UpdateUserMessage(ctx context.Context, claim app.Claim, content string) error {
	if !validClaim(claim) || claim.Incoming.Kind != domain.MessageText || strings.TrimSpace(content) == "" ||
		len(content) > app.MaxConversationTextBytes || !utf8.ValidString(content) || strings.ContainsRune(content, '\x00') {
		return ErrDatabaseConfiguration
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	conversation, err := loadConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return err
	}
	update, err := loadUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return ErrLeaseLost
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE conversation_messages SET content = ?
		WHERE chat_id = ? AND thread_id = ? AND update_id = ? AND role = 'user'`,
		content, claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, claim.Incoming.UpdateID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if rows != 1 {
		return ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) ModelContext(ctx context.Context, claim app.Claim) (domain.ModelContext, error) {
	if !validClaim(claim) || claim.Incoming.Kind != domain.MessageText {
		return domain.ModelContext{}, ErrLeaseLost
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return domain.ModelContext{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	conversation, err := loadConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return domain.ModelContext{}, err
	}
	update, err := loadUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return domain.ModelContext{}, safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return domain.ModelContext{}, ErrLeaseLost
	}
	var modelContext domain.ModelContext
	var itemsJSON string
	err = tx.QueryRowContext(ctx, `
		SELECT through_message_order, items_json
		FROM model_contexts WHERE chat_id = ? AND thread_id = ?`,
		claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID).Scan(
		&modelContext.ThroughMessageOrder, &itemsJSON)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return domain.ModelContext{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	if errors.Is(err, sql.ErrNoRows) {
		modelContext.ThroughMessageOrder = 0
		modelContext.ItemsJSON = json.RawMessage("[]")
		modelContext.Initialized = false
	} else {
		modelContext.ItemsJSON = json.RawMessage(itemsJSON)
		modelContext.Initialized = true
	}
	if err := tx.Commit(ctx); err != nil {
		return domain.ModelContext{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return modelContext, nil
}

func (s *Store) CommitResponse(ctx context.Context, claim app.Claim, response string, modelContext domain.ModelContext) error {
	if claim.Incoming.Kind != domain.MessageText {
		return ErrLeaseLost
	}
	return s.commitResponse(ctx, claim, response, modelContext)
}

func (s *Store) CommitAbout(ctx context.Context, claim app.Claim, response string) error {
	if claim.Incoming.Kind != domain.MessageAbout {
		return ErrLeaseLost
	}
	return s.commitResponse(ctx, claim, response, domain.ModelContext{})
}

func (s *Store) commitResponse(ctx context.Context, claim app.Claim, response string, modelContext domain.ModelContext) error {
	if !validClaim(claim) || strings.TrimSpace(response) == "" || len(response) > app.MaxConversationTextBytes || !utf8.ValidString(response) || strings.ContainsRune(response, '\x00') {
		return ErrDatabaseConfiguration
	}
	if claim.Incoming.Kind == domain.MessageText && (!modelContext.Initialized || len(modelContext.ItemsJSON) == 0 || !json.Valid(modelContext.ItemsJSON) || modelContext.ItemsJSON[0] != '[') {
		return ErrDatabaseConfiguration
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	if err := touchConversation(ctx, tx, claim.Incoming.Key); err != nil {
		return err
	}
	conversation, err := loadConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return err
	}
	update, err := loadUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return ErrLeaseLost
	}
	if claim.Incoming.Kind == domain.MessageText {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO conversation_messages (chat_id, thread_id, update_id, archive_order, role, content)
			VALUES (?, ?, ?, (SELECT COALESCE(MAX(archive_order), 0) + 1 FROM conversation_messages WHERE chat_id = ? AND thread_id = ?), 'assistant', ?)`,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, claim.Incoming.UpdateID,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, response); err != nil {
			return databaseError(ctx, err, ErrDatabaseOperation)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(archive_order), 0) FROM conversation_messages WHERE chat_id = ? AND thread_id = ?`,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID).Scan(&modelContext.ThroughMessageOrder); err != nil {
			return databaseError(ctx, err, ErrDatabaseOperation)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO model_contexts (chat_id, thread_id, through_message_order, items_json)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (chat_id, thread_id) DO UPDATE SET
				through_message_order = excluded.through_message_order,
				items_json = excluded.items_json,
				updated_at = julianday('now')`,
			claim.Incoming.Key.ChatID, claim.Incoming.Key.ThreadID, modelContext.ThroughMessageOrder, string(modelContext.ItemsJSON)); err != nil {
			return databaseError(ctx, err, ErrDatabaseOperation)
		}
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE processed_updates
		SET state = 'ready', response_text = ?, updated_at = julianday('now')
		WHERE update_id = ? AND state = 'processing'
		  AND processing_owner = ? AND processing_version = ?`,
		response, claim.Incoming.UpdateID, claim.OwnerToken, claim.Version)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrLeaseLost
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) ClaimDelivery(ctx context.Context, updateID int64, leaseDuration time.Duration) (app.DeliveryResult, error) {
	if updateID < 0 || !validateDuration(leaseDuration) {
		return app.DeliveryResult{}, ErrDatabaseConfiguration
	}
	key, exists, err := s.findUpdateKey(ctx, updateID)
	if err != nil {
		return app.DeliveryResult{}, err
	}
	if !exists {
		return app.DeliveryResult{}, ErrUpdateNotFound
	}
	owner, err := ownerToken()
	if err != nil {
		return app.DeliveryResult{}, ErrDatabaseOperation
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return app.DeliveryResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	if err := touchConversation(ctx, tx, key); err != nil {
		return app.DeliveryResult{}, err
	}
	conversation, err := loadConversation(ctx, tx, key)
	if err != nil {
		return app.DeliveryResult{}, err
	}
	update, err := loadUpdate(ctx, tx, updateID)
	if err != nil {
		return app.DeliveryResult{}, safeQueryError(ctx, err)
	}
	if (domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}) != key {
		return app.DeliveryResult{}, ErrUpdateConflict
	}
	result := app.DeliveryResult{}
	switch update.State {
	case "ready":
		if !update.ResponseText.Valid || strings.TrimSpace(update.ResponseText.String) == "" {
			return app.DeliveryResult{}, ErrDatabaseOperation
		}
		command, err := tx.ExecContext(ctx, `
			UPDATE processed_updates
			SET state = 'sending', delivery_owner = ?,
			    delivery_lease_until = julianday('now') + (? / ?),
			    updated_at = julianday('now')
			WHERE update_id = ? AND state = 'ready'`, owner,
			float64(leaseDuration.Microseconds()), float64(microsecondsPerDay), updateID)
		if err != nil {
			return app.DeliveryResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		if !oneRowAffected(command) {
			return app.DeliveryResult{}, ErrLeaseLost
		}
		result = app.DeliveryResult{
			Status: app.DeliveryClaimed,
			Delivery: app.Delivery{
				UpdateID: update.UpdateID, OwnerToken: owner, ProcessingOwner: update.ProcessingOwner,
				Outgoing: domain.Outgoing{
					Key:              domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID},
					ReplyToMessageID: update.SourceMessageID, Text: update.ResponseText.String,
				},
			},
		}
	case "sending":
		if update.DeliveryLeaseUntil.Valid && update.DeliveryLeaseUntil.Float64 > conversation.Now {
			result.Status = app.DeliveryBusy
		} else {
			if err := markUnknownTx(ctx, tx, update); err != nil {
				return app.DeliveryResult{}, err
			}
			if err := releaseConversation(ctx, tx, key, update.ProcessingOwner, update.ProcessingVersion); err != nil && !errors.Is(err, ErrLeaseLost) {
				return app.DeliveryResult{}, err
			}
			result.Status = app.DeliveryUnknownState
		}
	case "sent":
		result.Status = app.DeliverySent
	case "delivery_unknown":
		result.Status = app.DeliveryUnknownState
	default:
		result.Status = app.DeliveryBusy
	}
	if err := tx.Commit(ctx); err != nil {
		return app.DeliveryResult{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return result, nil
}

func (s *Store) MarkSent(ctx context.Context, delivery app.Delivery, telegramMessageID int64) error {
	if !validDelivery(delivery) || telegramMessageID <= 0 {
		return ErrDatabaseConfiguration
	}
	return s.finishDelivery(ctx, delivery, "sent", telegramMessageID)
}

func (s *Store) MarkDeliveryUnknown(ctx context.Context, delivery app.Delivery) error {
	if !validDelivery(delivery) {
		return ErrDatabaseConfiguration
	}
	return s.finishDelivery(ctx, delivery, "delivery_unknown", 0)
}

func (s *Store) finishDelivery(ctx context.Context, delivery app.Delivery, state string, telegramMessageID int64) error {
	key, exists, err := s.findUpdateKey(ctx, delivery.UpdateID)
	if err != nil {
		return err
	}
	if !exists {
		return ErrUpdateNotFound
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	if err := touchConversation(ctx, tx, key); err != nil {
		return err
	}
	update, err := loadUpdate(ctx, tx, delivery.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !matchesDelivery(update, delivery) {
		return ErrLeaseLost
	}
	var result sql.Result
	if state == "sent" {
		result, err = tx.ExecContext(ctx, `
			UPDATE processed_updates
			SET state = 'sent', telegram_message_id = ?, delivery_owner = NULL,
			    delivery_lease_until = NULL, updated_at = julianday('now')
			WHERE update_id = ? AND state = 'sending' AND delivery_owner = ?`,
			telegramMessageID, delivery.UpdateID, delivery.OwnerToken)
	} else {
		result, err = tx.ExecContext(ctx, `
			UPDATE processed_updates
			SET state = 'delivery_unknown', delivery_owner = NULL,
			    delivery_lease_until = NULL, updated_at = julianday('now')
			WHERE update_id = ? AND state = 'sending' AND delivery_owner = ?`,
			delivery.UpdateID, delivery.OwnerToken)
	}
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrLeaseLost
	}
	if err := advanceLastUpdate(ctx, tx, key, delivery.UpdateID); err != nil {
		return err
	}
	if err := releaseConversation(ctx, tx, key, delivery.ProcessingOwner, update.ProcessingVersion); err != nil && !errors.Is(err, ErrLeaseLost) {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) Release(ctx context.Context, claim app.Claim) error {
	if !validClaim(claim) {
		return ErrLeaseLost
	}
	tx, err := s.begin(ctx)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	defer func() { _ = tx.Rollback() }()
	if err := touchConversation(ctx, tx, claim.Incoming.Key); err != nil {
		return err
	}
	conversation, err := loadConversation(ctx, tx, claim.Incoming.Key)
	if err != nil {
		return err
	}
	update, err := loadUpdate(ctx, tx, claim.Incoming.UpdateID)
	if err != nil {
		return safeQueryError(ctx, err)
	}
	if !claimOwns(conversation, update, claim) {
		return ErrLeaseLost
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM processed_updates
		WHERE update_id = ? AND state = 'processing'
		  AND processing_owner = ? AND processing_version = ?`,
		claim.Incoming.UpdateID, claim.OwnerToken, claim.Version)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrLeaseLost
	}
	if err := releaseConversation(ctx, tx, claim.Incoming.Key, claim.OwnerToken, claim.Version); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) beginExisting(ctx context.Context, tx *transaction, conversation conversationRecord, update updateRecord, owner string, leaseDuration time.Duration) (app.BeginResult, error) {
	claim := claimFromRecord(update)
	switch update.State {
	case "processing":
		if update.Kind == "text" {
			if err := insertArchivedUserMessage(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.UpdateID, update.InputText); err != nil {
				return app.BeginResult{}, err
			}
		}
		if leaseActive(conversation) {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, nil
		}
		version := conversation.Version + 1
		key := domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}
		if err := setConversationLease(ctx, tx, key, version, owner, leaseDuration); err != nil {
			return app.BeginResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE processed_updates
			SET processing_owner = ?, processing_version = ?, updated_at = julianday('now')
			WHERE update_id = ? AND state = 'processing'`, owner, version, update.UpdateID); err != nil {
			return app.BeginResult{}, databaseError(ctx, err, ErrDatabaseOperation)
		}
		claim.OwnerToken, claim.Version = owner, version
		return app.BeginResult{Status: app.BeginClaimed, Claim: claim}, nil
	case "ready":
		return app.BeginResult{Status: app.BeginReady, Claim: claim}, nil
	case "sending":
		if update.DeliveryLeaseUntil.Valid && update.DeliveryLeaseUntil.Float64 > conversation.Now {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, nil
		}
		if err := markUnknownTx(ctx, tx, update); err != nil {
			return app.BeginResult{}, err
		}
		if err := releaseConversation(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.ProcessingOwner, update.ProcessingVersion); err != nil && !errors.Is(err, ErrLeaseLost) {
			return app.BeginResult{}, err
		}
		return app.BeginResult{Status: app.BeginUnknown, Claim: claim}, nil
	case "sent":
		return app.BeginResult{Status: app.BeginSent, Claim: claim}, nil
	case "delivery_unknown":
		return app.BeginResult{Status: app.BeginUnknown, Claim: claim}, nil
	default:
		return app.BeginResult{}, ErrDatabaseOperation
	}
}

func (s *Store) beginEarlierPending(ctx context.Context, tx *transaction, conversation *conversationRecord, update updateRecord, owner string, leaseDuration time.Duration) (app.BeginResult, bool, error) {
	claim := claimFromRecord(update)
	switch update.State {
	case "processing":
		if update.Kind == "text" {
			if err := insertArchivedUserMessage(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.UpdateID, update.InputText); err != nil {
				return app.BeginResult{}, false, err
			}
		}
		if leaseActive(*conversation) {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, true, nil
		}
		version := conversation.Version + 1
		key := domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}
		if err := setConversationLease(ctx, tx, key, version, owner, leaseDuration); err != nil {
			return app.BeginResult{}, false, err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE processed_updates
			SET processing_owner = ?, processing_version = ?, updated_at = julianday('now')
			WHERE update_id = ? AND state = 'processing'`, owner, version, update.UpdateID); err != nil {
			return app.BeginResult{}, false, databaseError(ctx, err, ErrDatabaseOperation)
		}
		claim.OwnerToken, claim.Version = owner, version
		return app.BeginResult{Status: app.BeginClaimed, Claim: claim, RetryCurrent: true}, true, nil
	case "ready":
		return app.BeginResult{Status: app.BeginReady, Claim: claim, RetryCurrent: true}, true, nil
	case "sending":
		if update.DeliveryLeaseUntil.Valid && update.DeliveryLeaseUntil.Float64 > conversation.Now {
			return app.BeginResult{Status: app.BeginBusy, Claim: claim}, true, nil
		}
		if err := markUnknownTx(ctx, tx, update); err != nil {
			return app.BeginResult{}, false, err
		}
		key := domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}
		releaseErr := releaseConversation(ctx, tx, key, update.ProcessingOwner, update.ProcessingVersion)
		if releaseErr != nil && !errors.Is(releaseErr, ErrLeaseLost) {
			return app.BeginResult{}, false, releaseErr
		}
		refreshed, err := loadConversation(ctx, tx, key)
		if err != nil {
			return app.BeginResult{}, false, err
		}
		*conversation = refreshed
		if errors.Is(releaseErr, ErrLeaseLost) && leaseActive(*conversation) {
			return app.BeginResult{Status: app.BeginBusy}, true, nil
		}
		return app.BeginResult{}, false, nil
	default:
		return app.BeginResult{}, false, ErrDatabaseOperation
	}
}

func insertArchivedUserMessage(ctx context.Context, tx *transaction, key domain.ConversationKey, updateID int64, text string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_messages (chat_id, thread_id, update_id, archive_order, role, content)
		VALUES (?, ?, ?, (SELECT COALESCE(MAX(archive_order), 0) + 1 FROM conversation_messages WHERE chat_id = ? AND thread_id = ?), 'user', ?)
		ON CONFLICT (chat_id, thread_id, update_id, role) DO NOTHING`,
		key.ChatID, key.ThreadID, updateID, key.ChatID, key.ThreadID, text); err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func cleanupStale(ctx context.Context, tx *transaction) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM processed_updates
		WHERE update_id IN (
			SELECT update_id FROM processed_updates
			WHERE state IN ('sent', 'delivery_unknown')
			AND updated_at < julianday('now') - ?
		ORDER BY updated_at LIMIT ?
		)`, processedUpdateRetention.Hours()/24, cleanupBatchSize)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func loadConversation(ctx context.Context, tx *transaction, key domain.ConversationKey) (conversationRecord, error) {
	var record conversationRecord
	err := tx.QueryRowContext(ctx, `
		SELECT version, lease_owner, lease_until, julianday('now')
		FROM conversations WHERE chat_id = ? AND thread_id = ?`, key.ChatID, key.ThreadID).Scan(
		&record.Version, &record.LeaseOwner, &record.LeaseUntil, &record.Now)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return conversationRecord{}, ErrUpdateNotFound
		}
		return conversationRecord{}, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return record, nil
}

func loadUpdate(ctx context.Context, tx *transaction, updateID int64) (updateRecord, error) {
	var record updateRecord
	err := tx.QueryRowContext(ctx, `
		SELECT update_id, chat_id, thread_id, source_message_id, input_text, kind, state,
		       processing_owner, processing_version, response_text, telegram_message_id,
		       delivery_owner, delivery_lease_until
		FROM processed_updates WHERE update_id = ?`, updateID).Scan(
		&record.UpdateID, &record.ChatID, &record.ThreadID, &record.SourceMessageID,
		&record.InputText, &record.Kind, &record.State, &record.ProcessingOwner,
		&record.ProcessingVersion, &record.ResponseText, &record.TelegramMessageID,
		&record.DeliveryOwner, &record.DeliveryLeaseUntil)
	return record, err
}

func loadPending(ctx context.Context, tx *transaction, key domain.ConversationKey, currentUpdateID int64) (updateRecord, error) {
	var record updateRecord
	err := tx.QueryRowContext(ctx, `
		SELECT update_id, chat_id, thread_id, source_message_id, input_text, kind, state,
		       processing_owner, processing_version, response_text, telegram_message_id,
		       delivery_owner, delivery_lease_until
		FROM processed_updates
		WHERE chat_id = ? AND thread_id = ? AND update_id <> ?
		  AND state IN ('processing', 'ready', 'sending')
		ORDER BY update_id LIMIT 1`, key.ChatID, key.ThreadID, currentUpdateID).Scan(
		&record.UpdateID, &record.ChatID, &record.ThreadID, &record.SourceMessageID,
		&record.InputText, &record.Kind, &record.State, &record.ProcessingOwner,
		&record.ProcessingVersion, &record.ResponseText, &record.TelegramMessageID,
		&record.DeliveryOwner, &record.DeliveryLeaseUntil)
	return record, err
}

func touchConversation(ctx context.Context, tx *transaction, key domain.ConversationKey) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE conversations SET updated_at = julianday('now')
		WHERE chat_id = ? AND thread_id = ?`, key.ChatID, key.ThreadID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrUpdateNotFound
	}
	return nil
}

func setConversationLease(ctx context.Context, tx *transaction, key domain.ConversationKey, version int64, owner string, duration time.Duration) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET version = ?, lease_owner = ?,
		    lease_until = julianday('now') + (? / ?), updated_at = julianday('now')
		WHERE chat_id = ? AND thread_id = ?`, version, owner,
		float64(duration.Microseconds()), float64(microsecondsPerDay), key.ChatID, key.ThreadID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrUpdateNotFound
	}
	return nil
}

func releaseConversation(ctx context.Context, tx *transaction, key domain.ConversationKey, owner string, version int64) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET version = version + 1, lease_owner = NULL, lease_until = NULL, updated_at = julianday('now')
		WHERE chat_id = ? AND thread_id = ? AND lease_owner = ? AND version = ?`, key.ChatID, key.ThreadID, owner, version)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrLeaseLost
	}
	return nil
}

func markUnknownTx(ctx context.Context, tx *transaction, update updateRecord) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE processed_updates
		SET state = 'delivery_unknown', delivery_owner = NULL,
		    delivery_lease_until = NULL, updated_at = julianday('now')
		WHERE update_id = ? AND state = 'sending'`, update.UpdateID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	if !oneRowAffected(result) {
		return ErrLeaseLost
	}
	return advanceLastUpdate(ctx, tx, domain.ConversationKey{ChatID: update.ChatID, ThreadID: update.ThreadID}, update.UpdateID)
}

func advanceLastUpdate(ctx context.Context, tx *transaction, key domain.ConversationKey, updateID int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE conversations
		SET last_update_id = CASE WHEN last_update_id < ? THEN ? ELSE last_update_id END,
		    updated_at = julianday('now')
		WHERE chat_id = ? AND thread_id = ?`, updateID, updateID, key.ChatID, key.ThreadID)
	if err != nil {
		return databaseError(ctx, err, ErrDatabaseOperation)
	}
	return nil
}

func (s *Store) findUpdateKey(ctx context.Context, updateID int64) (domain.ConversationKey, bool, error) {
	var key domain.ConversationKey
	err := s.db.QueryRowContext(ctx, `SELECT chat_id, thread_id FROM processed_updates WHERE update_id = ?`, updateID).
		Scan(&key.ChatID, &key.ThreadID)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ConversationKey{}, false, nil
	}
	if err != nil {
		return domain.ConversationKey{}, false, databaseError(ctx, err, ErrDatabaseOperation)
	}
	return key, true, nil
}

func validDatabaseURL(databaseURL string) bool {
	parsed, err := url.Parse(databaseURL)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" || parsed.Opaque != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	switch parsed.Scheme {
	case "turso", "libsql", "https":
		return true
	default:
		return false
	}
}

func validToken(token string) bool {
	if token == "" || len(token) > 4096 || strings.TrimSpace(token) != token {
		return false
	}
	for _, character := range token {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

func isConstraintError(err error) bool {
	var driverError *turso.Error
	if errors.As(err, &driverError) {
		return strings.Contains(driverError.Code, "CONSTRAINT_UNIQUE") || strings.Contains(driverError.Code, "CONSTRAINT_PRIMARYKEY")
	}
	if err == nil {
		return false
	}
	upper := strings.ToUpper(err.Error())
	return strings.Contains(upper, "UNIQUE CONSTRAINT FAILED") || strings.Contains(upper, "PRIMARY KEY CONSTRAINT FAILED")
}

func validClaim(claim app.Claim) bool {
	if !validIncoming(claim.Incoming) || claim.Version <= 0 || len(claim.OwnerToken) != 32 {
		return false
	}
	_, err := hex.DecodeString(claim.OwnerToken)
	return err == nil
}

func claimOwns(conversation conversationRecord, update updateRecord, claim app.Claim) bool {
	return leaseActive(conversation) && conversation.Version == claim.Version &&
		conversation.LeaseOwner.Valid && conversation.LeaseOwner.String == claim.OwnerToken &&
		update.State == "processing" && update.ProcessingOwner == claim.OwnerToken &&
		update.ProcessingVersion == claim.Version && matchesIncoming(update, claim.Incoming)
}

func validDelivery(delivery app.Delivery) bool {
	return delivery.UpdateID >= 0 && validOwnerToken(delivery.OwnerToken) && validOwnerToken(delivery.ProcessingOwner) &&
		delivery.Outgoing.Key.ChatID != 0 && delivery.Outgoing.Key.ThreadID >= 0 &&
		delivery.Outgoing.ReplyToMessageID > 0 && strings.TrimSpace(delivery.Outgoing.Text) != "" &&
		utf8.ValidString(delivery.Outgoing.Text) && !strings.ContainsRune(delivery.Outgoing.Text, '\x00')
}

func validOwnerToken(owner string) bool {
	if len(owner) != 32 {
		return false
	}
	_, err := hex.DecodeString(owner)
	return err == nil
}

func matchesDelivery(update updateRecord, delivery app.Delivery) bool {
	return update.State == "sending" && update.DeliveryOwner.Valid && update.DeliveryOwner.String == delivery.OwnerToken &&
		update.ProcessingOwner == delivery.ProcessingOwner && update.ChatID == delivery.Outgoing.Key.ChatID &&
		update.ThreadID == delivery.Outgoing.Key.ThreadID && update.SourceMessageID == delivery.Outgoing.ReplyToMessageID &&
		update.ResponseText.Valid && update.ResponseText.String == delivery.Outgoing.Text
}

func validIncoming(incoming domain.Incoming) bool {
	return incoming.UpdateID >= 0 && incoming.Key.ChatID != 0 && incoming.Key.ThreadID >= 0 &&
		incoming.MessageID > 0 && strings.TrimSpace(incoming.Text) != "" &&
		len(incoming.Text) <= app.MaxConversationTextBytes && utf8.ValidString(incoming.Text) &&
		!strings.ContainsRune(incoming.Text, '\x00') &&
		(incoming.Kind == domain.MessageText || incoming.Kind == domain.MessageAbout)
}

func matchesIncoming(record updateRecord, incoming domain.Incoming) bool {
	return record.ChatID == incoming.Key.ChatID && record.ThreadID == incoming.Key.ThreadID &&
		record.SourceMessageID == incoming.MessageID && record.InputText == incoming.Text &&
		record.Kind == kindName(incoming.Kind)
}

func kindName(kind domain.MessageKind) string {
	if kind == domain.MessageAbout {
		return "about"
	}
	return "text"
}

func incomingFromRecord(record updateRecord) domain.Incoming {
	kind := domain.MessageText
	if record.Kind == "about" {
		kind = domain.MessageAbout
	}
	return domain.Incoming{
		UpdateID:  record.UpdateID,
		Key:       domain.ConversationKey{ChatID: record.ChatID, ThreadID: record.ThreadID},
		MessageID: record.SourceMessageID,
		Text:      record.InputText,
		Kind:      kind,
	}
}

func claimFromIncoming(incoming domain.Incoming, owner string, version int64) app.Claim {
	return app.Claim{Incoming: incoming, OwnerToken: owner, Version: version}
}

func claimFromRecord(record updateRecord) app.Claim {
	return claimFromIncoming(incomingFromRecord(record), record.ProcessingOwner, record.ProcessingVersion)
}

func leaseActive(conversation conversationRecord) bool {
	return conversation.LeaseUntil.Valid && conversation.LeaseUntil.Float64 > conversation.Now
}

func ownerToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func oneRowAffected(result sql.Result) bool {
	if result == nil {
		return false
	}
	rowsAffected, err := result.RowsAffected()
	return err == nil && rowsAffected == 1
}

func safeQueryError(ctx context.Context, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrUpdateNotFound
	}
	return databaseError(ctx, err, ErrDatabaseOperation)
}

func validateDuration(duration time.Duration) bool {
	return duration > 0 && duration <= time.Hour
}

func databaseError(ctx context.Context, err error, fallback error) error {
	if err == nil {
		return nil
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return fallback
}
