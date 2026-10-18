package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/PlumpAlbert/payplump-bot/internal/clientmenu"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/internal/telegram"
	"github.com/jackc/pgx/v5"
)

// LoadMenuSnapshot checks deduplication before reading private account data, so
// an uncertain commit's redelivery does not need another remote billing read.
// The Telegram sender is the only accepted account selector.
func (s *Store) LoadMenuSnapshot(ctx context.Context, botID, updateID, actorTG int64) (clientmenu.Snapshot, bool, error) {
	if botID <= 0 || updateID < 0 || actorTG <= 0 {
		return clientmenu.Snapshot{}, false, errors.New("invalid client menu identity")
	}
	var duplicate bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (
        SELECT 1 FROM telegram_processed_updates WHERE bot_id = $1 AND update_id = $2
    )`, botID, updateID).Scan(&duplicate); err != nil {
		return clientmenu.Snapshot{}, false, menuStageError("check client menu delivery", err)
	}
	if duplicate {
		return clientmenu.Snapshot{}, true, nil
	}
	snapshot, err := readMenuSnapshot(ctx, s.pool, actorTG)
	return snapshot, false, err
}

// ApplyMenuUpdate commits only a reply whose actor scope and entire local
// snapshot are still valid. External reads/rendering happen before this method;
// no remote call or operation FIFO is involved in this short transaction.
func (s *Store) ApplyMenuUpdate(ctx context.Context, botID int64, botUsername string, update telegram.Update, prepared clientmenu.Prepared) error {
	if botID <= 0 || botUsername == "" || update.ID < 0 {
		return errors.New("invalid Telegram update identity")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return menuStageError("begin client menu update", err)
	}
	defer rollback(tx)
	// Keep exactly the Telegram update lock order: prepare bot, lock bot
	// without blocking FK checks, then record deduplication, then lock client.
	if _, err := tx.Exec(ctx, `INSERT INTO telegram_bots (bot_id) VALUES ($1) ON CONFLICT DO NOTHING`, botID); err != nil {
		return menuStageError("prepare client menu bot", err)
	}
	var lockedBotID int64
	if err := tx.QueryRow(ctx, `SELECT bot_id FROM telegram_bots WHERE bot_id = $1 FOR NO KEY UPDATE`, botID).Scan(&lockedBotID); err != nil {
		return menuStageError("lock client menu bot", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO telegram_processed_updates (bot_id, update_id) VALUES ($1, $2) ON CONFLICT DO NOTHING`, botID, update.ID)
	if err != nil {
		return menuStageError("record client menu delivery", err)
	}
	if tag.RowsAffected() == 0 {
		// Do not validate changed bodies/proofs after an uncertain commit.
		return menuStageError("commit duplicate client menu update", tx.Commit(ctx))
	}
	identity, action, supported := update.PrivateMenu(botUsername)
	if !supported {
		return menuStageError("commit unsupported client menu update", tx.Commit(ctx))
	}
	request := clientmenu.Request{
		BotID: botID, UpdateID: update.ID, TelegramID: identity.TelegramID,
		ChatID: identity.ChatID, Action: action,
	}
	if !request.Valid() || prepared.Request != request {
		return errors.New("client menu preparation does not match Telegram update")
	}
	if err := lockMenuAccount(ctx, tx, identity.TelegramID); err != nil {
		return err
	}
	actual, err := readMenuSnapshot(ctx, tx, identity.TelegramID)
	if err != nil {
		return err
	}
	if actual != prepared.Snapshot {
		return clientmenu.ErrStaleSnapshot
	}
	if actual.Known {
		if err := recordBroadcastMembership(ctx, tx, botID, identity.TelegramID); err != nil {
			return menuStageError("record broadcast membership", err)
		}
	}
	if prepared.ProtectedConfiguration {
		if err := enqueueConfigurationIntent(ctx, tx, prepared); err != nil {
			return err
		}
		return menuStageError("commit protected configuration request", tx.Commit(ctx))
	}
	if prepared.Text == queue.ConfigurationIntentBody {
		return errors.New("invalid protected configuration intent")
	}
	if action == clientmenu.Configuration {
		// Configuration bodies are never accepted as ordinary prepared text.
		// A disabled/ineligible request gets only this fixed, nonsensitive reply.
		if _, err := enqueueOutbox(ctx, tx, queue.Draft{
			BotID: botID, ChatID: identity.ChatID, Key: request.Key(), Text: configurationDeniedText, ClientMenu: true,
		}); err != nil {
			return menuStageError("enqueue configuration denial", err)
		}
		return menuStageError("commit configuration denial", tx.Commit(ctx))
	}
	if action == clientmenu.Start {
		if !actual.Known {
			registered, err := registerPrivateClient(ctx, tx, TelegramIdentity{
				TelegramID: identity.TelegramID, ChatID: identity.ChatID, DisplayName: identity.DisplayName,
			})
			if err != nil {
				return menuStageError("register client menu sender", err)
			}
			// A concurrent insert can win after the unknown snapshot was read.
			// Created alone proves only subscription creation, not ownership of
			// the new client; both inserts must belong to this transaction.
			if !registered.ClientCreated || !registered.Created {
				return clientmenu.ErrStaleSnapshot
			}
			if err := recordBroadcastMembership(ctx, tx, botID, identity.TelegramID); err != nil {
				return menuStageError("record broadcast membership", err)
			}
		} else {
			if _, err := tx.Exec(ctx, `UPDATE clients SET chat_id = $2, display_name = $3 WHERE telegram_id = $1`,
				identity.TelegramID, identity.ChatID, identity.DisplayName); err != nil {
				return menuStageError("refresh client menu profile", err)
			}
		}
	}
	if _, err := enqueueOutbox(ctx, tx, queue.Draft{
		BotID: botID, ChatID: identity.ChatID, Key: request.Key(), Text: prepared.Text, ClientMenu: true,
	}); err != nil {
		return menuStageError("enqueue client menu reply", err)
	}
	return menuStageError("commit client menu update", tx.Commit(ctx))
}

type menuSnapshotReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// A single projection observes all account bindings coherently. LEFT JOIN is
// deliberate: a client with a missing subscription is corrupt, not unknown.
func readMenuSnapshot(ctx context.Context, reader menuSnapshotReader, actorTG int64) (clientmenu.Snapshot, error) {
	var snapshot clientmenu.Snapshot
	var chatID int64
	var subscriptionExists bool
	err := reader.QueryRow(ctx, `
        SELECT c.id, c.chat_id, COALESCE(c.invoice_ninja_id, ''),
            COALESCE(s.current_invoice_id, ''), COALESCE(c.sui_id, ''),
            COALESCE(s.recurring_id, ''), c.configuration_revision,
            s.client_id IS NOT NULL, COALESCE(s.state, ''), COALESCE(s.desired_access, false),
            COALESCE(s.payment_date::text, ''), COALESCE(s.next_invoice_date::text, ''),
            EXISTS (SELECT 1 FROM payment_confirmations pc WHERE pc.client_id=c.id
                AND pc.invoice_id=s.current_invoice_id AND pc.review_required)
        FROM clients c LEFT JOIN subscriptions s ON s.client_id = c.id
        WHERE c.telegram_id = $1`, actorTG).Scan(
		&snapshot.ClientID, &chatID, &snapshot.CustomerID, &snapshot.InvoiceID, &snapshot.SUIID,
		&snapshot.RecurringID, &snapshot.ConfigurationRevision,
		&subscriptionExists, &snapshot.State, &snapshot.DesiredAccess, &snapshot.PaymentDate, &snapshot.NextInvoiceDate, &snapshot.PaymentReview,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return clientmenu.Snapshot{}, nil
	}
	if err != nil {
		return clientmenu.Snapshot{}, menuStageError("read client menu account", err)
	}
	if snapshot.ClientID <= 0 || chatID != actorTG || !subscriptionExists ||
		(snapshot.State != "awaiting_payment" && snapshot.State != "active" && snapshot.State != "overdue") {
		return clientmenu.Snapshot{}, errors.New("invalid client menu account state")
	}
	snapshot.Known = true
	return snapshot, nil
}

func lockMenuAccount(ctx context.Context, tx pgx.Tx, actorTG int64) error {
	var clientID int64
	err := tx.QueryRow(ctx, `SELECT id FROM clients WHERE telegram_id = $1 FOR UPDATE`, actorTG).Scan(&clientID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return menuStageError("lock client menu account", err)
	}
	var subscriptionID int64
	err = tx.QueryRow(ctx, `SELECT client_id FROM subscriptions WHERE client_id = $1 FOR UPDATE`, clientID).Scan(&subscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("invalid client menu account state")
	}
	return menuStageError("lock client menu subscription", err)
}

// Driver errors can include SQL, message text and private bindings. Preserve
// only safe sentinels/context and report the local stage for everything else.
func menuStageError(stage string, err error) error {
	if err == nil {
		return nil
	}
	for _, sentinel := range []error{clientmenu.ErrStaleSnapshot, queue.ErrConflict, context.Canceled, context.DeadlineExceeded} {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("%s: %w", stage, sentinel)
		}
	}
	return errors.New(stage)
}
