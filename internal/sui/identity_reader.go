package sui

import (
	"context"
	"log/slog"
	"net/url"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// Identity is a private, redacted inventory hint, not a full record or proof
// of ownership. Use RecordReader.GetRecord for the complete authoritative snapshot.
type Identity struct {
	id      int64
	name    string
	enabled bool
}

func (Identity) String() string       { return "[private SUI identity]" }
func (Identity) GoString() string     { return "[private SUI identity]" }
func (Identity) LogValue() slog.Value { return slog.StringValue("[private SUI identity]") }
func (i Identity) ID() int64          { return i.id }
func (i Identity) Name() string       { return i.name }
func (i Identity) Enabled() bool      { return i.enabled }

// IdentityReader exposes only bounded identity inventory reads. It does not
// expose full records or any write capability.
type IdentityReader struct {
	http            *integrationhttp.Client
	declaredVersion string
}

// NewIdentityReader accepts the same pinned version claims as RecordReader.
// It constructs only a read-only HTTP client.
func NewIdentityReader(base, token, declaredVersion string, timeout time.Duration) (*IdentityReader, error) {
	if declaredVersion != "1.6.4" && declaredVersion != "v1.6.4" {
		return nil, errInvalidConfiguration
	}
	reader, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	return &IdentityReader{http: reader.http, declaredVersion: declaredVersion}, nil
}

func (c *IdentityReader) get(ctx context.Context, endpoint string, query url.Values) (map[string]any, error) {
	return getManaged(ctx, c.http, endpoint, query)
}

// ListIdentities returns a bounded, strictly validated identity inventory.
// These summaries are not records and must not authorize access or mutation.
func (c *IdentityReader) ListIdentities(ctx context.Context, maxAccounts int) ([]Identity, error) {
	if maxAccounts < 1 || maxAccounts > 1000 {
		return nil, ErrManagedInvalid
	}
	get := managedGetter(c.get)
	if err := checkManagedVersion(ctx, get, c.declaredVersion); err != nil {
		return nil, err
	}
	summaries, err := managedIdentitySummaries(ctx, get, maxAccounts)
	if err != nil {
		return nil, err
	}
	identities := make([]Identity, 0, len(summaries))
	for _, summary := range summaries {
		if !summary.hasEnabled {
			return nil, ErrManagedResponse
		}
		identities = append(identities, Identity{id: summary.id, name: summary.name, enabled: summary.enabled})
	}
	return identities, nil
}
