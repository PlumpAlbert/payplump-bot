// Package postgres owns durable bot state. No external API is called inside a
// database transaction: registration records work for a later reconciler.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

func Open(ctx context.Context, databaseURL string, maxConns int32) (*Store, error) {
	if databaseURL == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	if maxConns < 1 || maxConns > 64 {
		return nil, errors.New("DB_MAX_CONNS must be between 1 and 64")
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("invalid DATABASE_URL")
	}
	cfg.MaxConns = maxConns
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errors.New("could not initialize PostgreSQL pool")
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errors.New("PostgreSQL unavailable; check DATABASE_URL and connectivity")
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// TelegramIdentity must come from the sender of a private Telegram message.
// Names are presentation only; the stable sender ID is the identity key.
type TelegramIdentity struct {
	TelegramID  int64
	ChatID      int64
	DisplayName string
}

type Client struct {
	ID          int64
	TelegramID  int64
	ChatID      int64
	DisplayName string
}

type Registration struct {
	Client  Client
	Created bool
}

// RegisterPrivateClient atomically saves the identity, a single unpaid
// subscription, and a single durable registration operation. Concurrent and
// repeated /start calls cannot create a second subscription or operation.
func (s *Store) RegisterPrivateClient(ctx context.Context, identity TelegramIdentity) (Registration, error) {
	if identity.TelegramID <= 0 || identity.ChatID != identity.TelegramID {
		return Registration{}, errors.New("registration requires the sender's private chat")
	}
	if !utf8.ValidString(identity.DisplayName) || utf8.RuneCountInString(identity.DisplayName) > 256 {
		return Registration{}, errors.New("display name must be valid UTF-8 with at most 256 characters")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Registration{}, fmt.Errorf("begin registration: %w", err)
	}
	defer rollback(tx)
	var result Registration
	err = tx.QueryRow(ctx, `
        INSERT INTO clients (telegram_id, chat_id, display_name) VALUES ($1, $2, $3)
        ON CONFLICT (telegram_id) DO UPDATE
            SET chat_id = EXCLUDED.chat_id, display_name = EXCLUDED.display_name
        RETURNING id, telegram_id, chat_id, display_name`,
		identity.TelegramID, identity.ChatID, identity.DisplayName,
	).Scan(&result.Client.ID, &result.Client.TelegramID, &result.Client.ChatID, &result.Client.DisplayName)
	if err != nil {
		return Registration{}, fmt.Errorf("save client: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO subscriptions (client_id) VALUES ($1) ON CONFLICT (client_id) DO NOTHING`, result.Client.ID)
	if err != nil {
		return Registration{}, fmt.Errorf("save subscription: %w", err)
	}
	result.Created = tag.RowsAffected() == 1
	if result.Created {
		_, err = tx.Exec(ctx, `INSERT INTO operations (client_id, operation_key, kind) VALUES ($1, $2, 'register_client')`,
			result.Client.ID, fmt.Sprintf("register:%d", result.Client.ID))
		if err != nil {
			return Registration{}, fmt.Errorf("enqueue registration: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Registration{}, fmt.Errorf("commit registration: %w", err)
	}
	return result, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
