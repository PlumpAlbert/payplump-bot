package sui

import (
	"context"
	"net/url"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// RecordReader reads complete sensitive snapshots without constructing a writer.
// Version checks verify a claim only, not the installed build or local ownership.
// Subscription URLs in records are data: this adapter never fetches them.
type RecordReader struct {
	http            *integrationhttp.Client
	declaredVersion string
}

// NewRecordReader requires exactly 1.6.4 or v1.6.4; its status GET must report
// the identical spelling. HTTP permits private NetBird networks; HTTPS uses normal
// certificate verification. The installed panel prefix is preserved.
func NewRecordReader(base, token, declaredVersion string, timeout time.Duration) (*RecordReader, error) {
	if declaredVersion != "1.6.4" && declaredVersion != "v1.6.4" {
		return nil, errInvalidConfiguration
	}
	reader, err := New(base, token, timeout)
	if err != nil {
		return nil, err
	}
	return &RecordReader{http: reader.http, declaredVersion: declaredVersion}, nil
}

func (c *RecordReader) get(ctx context.Context, endpoint string, query url.Values) (map[string]any, error) {
	return getManaged(ctx, c.http, endpoint, query)
}

// GetRecord checks the version claim and reads exactly one full record by its
// canonical numeric ID. Summaries, unknown fields and duplicate JSON keys fail.
func (c *RecordReader) GetRecord(ctx context.Context, id int64) (ClientRecord, error) {
	if id <= 0 {
		return ClientRecord{}, ErrManagedInvalid
	}
	if err := checkManagedVersion(ctx, c.get, c.declaredVersion); err != nil {
		return ClientRecord{}, err
	}
	records, err := fullManagedRecords(ctx, c.get, []int64{id})
	if err != nil {
		return ClientRecord{}, err
	}
	return records[0], nil
}
