package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// SecretMessage represents a stored encrypted secret message.
type SecretMessage struct {
	ID                int64
	Token             string
	ChatID            int64
	MessageID         int64
	SenderID          int64
	RecipientID       int64
	RecipientUsername string
	EncryptedData     []byte
	Nonce             []byte
	FileID            string
	FileType          string
	InlineMessageID   string
	CreatedAt         time.Time
	ReadAt            *time.Time
}

// PostgresStorage provides PostgreSQL-backed storage for secret messages.
type PostgresStorage struct {
	conn *pgx.Conn
}

// NewPostgresStorage connects to PostgreSQL and runs schema migration.
func NewPostgresStorage(databaseURL string) (*PostgresStorage, error) {
	conn, err := pgx.Connect(context.Background(), databaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	if err := conn.Ping(context.Background()); err != nil {
		return nil, fmt.Errorf("ping database: %w", err)
	}

	// Run schema migration
	schema := `
	CREATE TABLE IF NOT EXISTS secret_messages (
		id SERIAL PRIMARY KEY,
		chat_id BIGINT NOT NULL,
		message_id BIGINT NOT NULL,
		sender_id BIGINT NOT NULL,
		recipient_id BIGINT NOT NULL,
		encrypted_data BYTEA NOT NULL,
		nonce BYTEA NOT NULL,
		created_at TIMESTAMPTZ DEFAULT NOW(),
		read_at TIMESTAMPTZ
	);
	`
	if _, err := conn.Exec(context.Background(), schema); err != nil {
		return nil, fmt.Errorf("run schema migration: %w", err)
	}

	// Add token column if upgrading from old schema
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE secret_messages ADD COLUMN IF NOT EXISTS token TEXT UNIQUE`,
	); err != nil {
		return nil, fmt.Errorf("add token column: %w", err)
	}

	// Add recipient_username column if upgrading from old schema
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE secret_messages ADD COLUMN IF NOT EXISTS recipient_username TEXT`,
	); err != nil {
		return nil, fmt.Errorf("add recipient_username column: %w", err)
	}

	// Create username cache table for @username resolution
	if _, err := conn.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS username_cache (
			username TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			updated_at TIMESTAMPTZ DEFAULT NOW()
		)`,
	); err != nil {
		return nil, fmt.Errorf("create username cache table: %w", err)
	}

	// Add file_id and file_type columns for photo/video delivery
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE secret_messages ADD COLUMN IF NOT EXISTS file_id TEXT`,
	); err != nil {
		return nil, fmt.Errorf("add file_id column: %w", err)
	}
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE secret_messages ADD COLUMN IF NOT EXISTS file_type TEXT`,
	); err != nil {
		return nil, fmt.Errorf("add file_type column: %w", err)
	}
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE secret_messages ADD COLUMN IF NOT EXISTS inline_message_id TEXT`,
	); err != nil {
		return nil, fmt.Errorf("add inline_message_id column: %w", err)
	}

	// Create media_uploads table for PV upload → inline token flow
	if _, err := conn.Exec(context.Background(),
		`CREATE TABLE IF NOT EXISTS media_uploads (
			token TEXT PRIMARY KEY,
			file_id TEXT NOT NULL,
			file_type TEXT NOT NULL,
			sender_id BIGINT NOT NULL,
			caption TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW()
		)`,
	); err != nil {
		return nil, fmt.Errorf("create media_uploads table: %w", err)
	}

	// Add caption column to media_uploads if upgrading from old schema
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE media_uploads ADD COLUMN IF NOT EXISTS caption TEXT NOT NULL DEFAULT ''`,
	); err != nil {
		return nil, fmt.Errorf("add media_uploads caption column: %w", err)
	}

	// Add first_name column to username_cache
	if _, err := conn.Exec(context.Background(),
		`ALTER TABLE username_cache ADD COLUMN IF NOT EXISTS first_name TEXT NOT NULL DEFAULT ''`,
	); err != nil {
		return nil, fmt.Errorf("add username_cache first_name column: %w", err)
	}

	return &PostgresStorage{conn: conn}, nil
}

// GenerateToken creates a short random hex token (16 chars from 8 bytes).
func GenerateToken() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// GenerateShortToken creates a short random hex token (8 chars from 4 bytes) for media uploads.
func GenerateShortToken() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate short token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// MediaUpload represents a media file uploaded to the bot in PV.
type MediaUpload struct {
	Token    string
	FileID   string
	FileType string
	SenderID int64
	Caption  string
}

// SaveMedia stores a media upload with its token and optional caption.
func (s *PostgresStorage) SaveMedia(ctx context.Context, token, fileID, fileType string, senderID int64, caption string) error {
	_, err := s.conn.Exec(ctx,
		`INSERT INTO media_uploads (token, file_id, file_type, sender_id, caption)
		 VALUES ($1, $2, $3, $4, $5)`,
		token, fileID, fileType, senderID, caption,
	)
	if err != nil {
		return fmt.Errorf("save media: %w", err)
	}
	return nil
}

// GetMediaByToken retrieves a media upload by its token.
func (s *PostgresStorage) GetMediaByToken(ctx context.Context, token string) (*MediaUpload, error) {
	m := &MediaUpload{}
	err := s.conn.QueryRow(ctx,
		`SELECT token, file_id, file_type, sender_id, caption FROM media_uploads WHERE token = $1`,
		token,
	).Scan(&m.Token, &m.FileID, &m.FileType, &m.SenderID, &m.Caption)
	if err != nil {
		return nil, fmt.Errorf("get media by token: %w", err)
	}
	return m, nil
}

// DeleteMedia removes a media upload after it has been used.
func (s *PostgresStorage) DeleteMedia(ctx context.Context, token string) error {
	_, err := s.conn.Exec(ctx,
		`DELETE FROM media_uploads WHERE token = $1`, token,
	)
	if err != nil {
		return fmt.Errorf("delete media: %w", err)
	}
	return nil
}

// SaveMessage stores an encrypted secret message.
// If recipientUsername is not empty, the message is addressed by @username instead of user ID.
// fileID and fileType are optional (empty for text-only messages).
func (s *PostgresStorage) SaveMessage(
	ctx context.Context,
	token string,
	chatID, senderID, recipientID int64,
	recipientUsername string,
	encryptedData, nonce []byte,
	fileID, fileType string,
) error {
	_, err := s.conn.Exec(ctx,
		`INSERT INTO secret_messages (token, chat_id, message_id, sender_id, recipient_id, recipient_username, encrypted_data, nonce, file_id, file_type)
		 VALUES ($1, $2, 0, $3, $4, $5, $6, $7, $8, $9)`,
		token, chatID, senderID, recipientID, recipientUsername, encryptedData, nonce, fileID, fileType,
	)
	if err != nil {
		return fmt.Errorf("save message: %w", err)
	}
	return nil
}

// GetMessageByToken retrieves a secret message by its unique token.
func (s *PostgresStorage) GetMessageByToken(ctx context.Context, token string) (*SecretMessage, error) {
	msg := &SecretMessage{}
	err := s.conn.QueryRow(ctx,
		`SELECT id, token, chat_id, message_id, sender_id, recipient_id, recipient_username, encrypted_data, nonce, file_id, file_type, inline_message_id, created_at, read_at
		 FROM secret_messages
		 WHERE token = $1`,
		token,
	).Scan(&msg.ID, &msg.Token, &msg.ChatID, &msg.MessageID, &msg.SenderID, &msg.RecipientID,
		&msg.RecipientUsername, &msg.EncryptedData, &msg.Nonce, &msg.FileID, &msg.FileType,
		&msg.InlineMessageID, &msg.CreatedAt, &msg.ReadAt)
	if err != nil {
		return nil, fmt.Errorf("get message by token: %w", err)
	}
	return msg, nil
}

// MarkAsReadByToken marks a secret message as read by its token.
func (s *PostgresStorage) MarkAsReadByToken(ctx context.Context, token string, readAt time.Time) error {
	_, err := s.conn.Exec(ctx,
		`UPDATE secret_messages SET read_at = $1 WHERE token = $2`,
		readAt, token,
	)
	if err != nil {
		return fmt.Errorf("mark as read: %w", err)
	}
	return nil
}

// UpdateInlineMessageID saves the inline_message_id for a given secret token.
// This allows the bot to later edit the inline message to show a read receipt.
func (s *PostgresStorage) UpdateInlineMessageID(ctx context.Context, token, inlineMessageID string) error {
	_, err := s.conn.Exec(ctx,
		`UPDATE secret_messages SET inline_message_id = $1 WHERE token = $2`,
		inlineMessageID, token,
	)
	if err != nil {
		return fmt.Errorf("update inline message id: %w", err)
	}
	return nil
}

// Close closes the database connection.
func (s *PostgresStorage) Close() error {
	return s.conn.Close(context.Background())
}

// SaveUsername stores or updates the mapping from a Telegram username to a user ID.
// firstName is the user's display name (can be empty).
func (s *PostgresStorage) SaveUsername(ctx context.Context, username string, userID int64, firstName string) error {
	_, err := s.conn.Exec(ctx,
		`INSERT INTO username_cache (username, user_id, first_name, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (username) DO UPDATE SET user_id = $2, first_name = $3, updated_at = NOW()`,
		username, userID, firstName,
	)
	if err != nil {
		return fmt.Errorf("save username: %w", err)
	}
	return nil
}

// GetUserDisplayName retrieves the first name of a user by their user ID.
// Returns ("", nil) if the user ID is not found.
func (s *PostgresStorage) GetUserDisplayName(ctx context.Context, userID int64) (string, error) {
	var firstName string
	err := s.conn.QueryRow(ctx,
		`SELECT COALESCE(first_name, '') FROM username_cache WHERE user_id = $1`,
		userID,
	).Scan(&firstName)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get user display name: %w", err)
	}
	return firstName, nil
}

// GetUserIDByUsername looks up a user ID by their Telegram username (without @).
// Returns (0, nil) if the username is not found.
func (s *PostgresStorage) GetUserIDByUsername(ctx context.Context, username string) (int64, error) {
	var userID int64
	err := s.conn.QueryRow(ctx,
		`SELECT user_id FROM username_cache WHERE username = $1`,
		username,
	).Scan(&userID)
	if err == pgx.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("get user id by username: %w", err)
	}
	return userID, nil
}

// GetUsernameByUserID looks up a username by user ID.
// Returns ("", nil) if the user ID is not found.
func (s *PostgresStorage) GetUsernameByUserID(ctx context.Context, userID int64) (string, error) {
	var username string
	err := s.conn.QueryRow(ctx,
		`SELECT username FROM username_cache WHERE user_id = $1`,
		userID,
	).Scan(&username)
	if err == pgx.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get username by user id: %w", err)
	}
	return username, nil
}
