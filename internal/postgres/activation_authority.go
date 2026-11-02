package postgres

import (
	"context"
	"errors"

	"github.com/PlumpAlbert/payplump-bot/internal/activation"
	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/jackc/pgx/v5"
)

// AuthorityActivationStore selects the explicit recurring-only journal. All
// dispatches still use Store's shared irrevocable reservation and receipt APIs.
type AuthorityActivationStore struct {
	*Store
	policy [32]byte
}

func NewAuthorityActivationStore(s *Store, cfg activation.Config) (*AuthorityActivationStore, error) {
	if s == nil || !cfg.Enabled || !cfg.NinjaAuthority {
		return nil, errors.New("authority activation requires explicit enabled mode")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &AuthorityActivationStore{Store: s, policy: cfg.PolicyHash()}, nil
}

func (s *AuthorityActivationStore) SeedActivations(ctx context.Context, bot int64, allowed []int64, policy [32]byte) (int, error) {
	if policy != s.policy {
		return 0, queue.ErrConflict
	}
	return s.Store.seedActivations(ctx, bot, allowed, policy, activation.NinjaAuthorityMode)
}
func (s *AuthorityActivationStore) LoadActivation(ctx context.Context, lease queue.Lease, bot int64) (activation.Snapshot, error) {
	return s.Store.loadActivation(ctx, lease, bot, &s.policy)
}

// Completing recurring provisioning is not completing the legacy SUI audit.
// Persist only the actual recurring receipt. Access state, dates, SUI identity
// and subsequent lifecycle decisions belong exclusively to the reconciler.
func finishAuthorityActivation(ctx context.Context, tx pgx.Tx, lease queue.Lease, actual activation.Snapshot) error {
	guard, err := lockActivationGuard(ctx, tx)
	if err != nil {
		return err
	}
	if guard.operationID != nil && *guard.operationID == lease.ID {
		return queue.ErrConflict
	}
	tag, err := tx.Exec(ctx, `UPDATE subscriptions SET recurring_id=$2
 WHERE client_id=$1 AND recurring_id IS NULL AND state='awaiting_payment' AND NOT desired_access`, actual.Account.ClientID, actual.Steps[activation.RecurringCreate].ExternalID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrConflict
	}
	if err := checkActivationDeadline(ctx, tx, actual); err != nil {
		return err
	}
	if _, err := readActivationLease(ctx, tx, lease); err != nil {
		return err
	}
	deadline, err := activationDeadline(actual)
	if err != nil {
		return err
	}
	tag, err = tx.Exec(ctx, `UPDATE operations SET status='completed',completed_at=clock_timestamp(),locked_until=NULL,last_error_code=0
 WHERE id=$1 AND attempts=$2 AND kind='activate_first_subscription' AND status='running'
 AND locked_until>clock_timestamp() AND clock_timestamp()<$3::timestamptz`, lease.ID, lease.Attempt, deadline)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return queue.ErrLeaseLost
	}
	return tx.Commit(ctx)
}

var _ activation.Store = (*AuthorityActivationStore)(nil)
