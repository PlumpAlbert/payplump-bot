package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestRecurringPostContradictoryEnvelopeIsUnknownNeverReplayed(t *testing.T) {
	data := string(recurringTestJSON(t, recurringTestRecord(t)))
	for _, body := range []string{`{"data":null,"data":` + data + `}`, `{"DATA":null,"data":` + data + `}`, `{"data":` + data + `,"meta":{"status":1,"ſtatus":2}}`} {
		posts := 0
		s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				t.Error("post boundary performed unexpected lookup")
			}
			posts++
			fmt.Fprint(w, body)
		})
		c := recurringTestClient(t, s.URL, 1, 1)
		var result json.RawMessage
		if err := c.post(context.Background(), []string{"api", "v1", "recurring_invoices"}, struct{}{}, &result); !errors.Is(err, ErrRecurringUnknown) || posts != 1 {
			t.Fatal("contradictory write receipt was accepted or retried")
		}
	}
}
