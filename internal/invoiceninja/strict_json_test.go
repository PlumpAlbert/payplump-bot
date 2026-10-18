package invoiceninja

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PlumpAlbert/payplump-bot/internal/integrationhttp"
)

// Include both Unicode fold cycles that alias ASCII encoding/json field names.
func ninjaJSONTestAliases(key string) []string {
	aliases := []string{key, strings.ToUpper(key)}
	if strings.Contains(key, "s") {
		aliases = append(aliases, strings.Replace(key, "s", "ſ", 1))
	}
	if strings.Contains(key, "k") {
		aliases = append(aliases, strings.Replace(key, "k", "K", 1))
	}
	return aliases
}

func ninjaJSONTestField(body, key, value string) string {
	return body[:len(body)-1] + fmt.Sprintf(",%q:%s}", key, value)
}

func TestUniqueNinjaJSON(t *testing.T) {
	for _, raw := range []string{
		`null`, `true`, `"value"`, `1e10000`, `9007199254740993.010000000000000001`,
		` {"s":1,"k":2,"未知":{"café":3,"σ":4},"é":5,"e":6} `,
		`[{"id":1},{"id":2}]`,
		strings.Repeat("[", 128) + "0" + strings.Repeat("]", 128),
	} {
		if !uniqueNinjaJSON([]byte(raw)) {
			t.Fatal("valid unique value rejected")
		}
	}
	for _, raw := range []string{
		``, ` `, `{"id":}`, `{"id":1,}`, `[1,]`, `{"id":1]`, `]`,
		`{} {}`, `{} null`, `true false`, `{} trailing`,
		`{"id":1,"id":2}`, `{"id":1,"ID":2}`, `{"id":1,"\u0069d":2}`,
		`{"s":1,"ſ":2}`, `{"S":1,"ſ":2}`, `{"k":1,"K":2}`,
		`{"σ":1,"ς":2}`, `{"Σ":1,"ς":2}`,
		`{"unknown":[{"S":1,"ſ":2}]}`,
		strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129),
		`"` + strings.Repeat("x", integrationhttp.MaxResponseBytes) + `"`,
	} {
		if uniqueNinjaJSON([]byte(raw)) {
			t.Fatal("ambiguous, incomplete or unbounded value accepted")
		}
	}
}

func TestRecurringUniqueJSONReaderAndDirectDTO(t *testing.T) {
	record := recurringTestRecord(t)
	record.StatusID = "2"
	base := string(recurringTestJSON(t, record))
	assertRejected := func(t *testing.T, body string) {
		t.Helper()
		var direct RecurringRecord
		if err := json.Unmarshal([]byte(body), &direct); err == nil {
			t.Fatal("direct DTO erased conflicting fields", err)
		}
		c, calls := recurringReaderServer(t, []byte(`{"data":`+body+`}`), SupportedRecurringVersion)
		if _, err := c.Get(context.Background(), record.ID); err == nil || calls.Load() != 1 {
			t.Fatal("reader accepted conflicting fields or retried", err)
		}
	}
	for key, value := range recurringTestMap(t, record) {
		for _, alias := range ninjaJSONTestAliases(key) {
			t.Run("record/"+alias, func(t *testing.T) {
				assertRejected(t, ninjaJSONTestField(base, alias, string(value)))
			})
		}
	}
	// The original vulnerability: an earlier DRAFT overwritten by ACTIVE.
	t.Run("conflicting-status", func(t *testing.T) {
		assertRejected(t, strings.Replace(base, `"status_id":"2"`, `"status_id":"1","status_id":"2"`, 1))
	})
	line := string(recurringTestJSON(t, record.LineItems[0]))
	for key, value := range recurringTestMap(t, record.LineItems[0]) {
		for _, alias := range ninjaJSONTestAliases(key) {
			t.Run("line/"+alias, func(t *testing.T) {
				body := strings.Replace(base, line, ninjaJSONTestField(line, alias, string(value)), 1)
				assertRejected(t, body)
			})
		}
	}
	for name, value := range map[string]string{
		"unknown-nested": `[{"amount":1,"AMOUNT":2}]`,
		"unicode-nested": `{"items":[{"k":1,"K":2}]}`,
		"too-deep":       strings.Repeat("[", 129) + "0" + strings.Repeat("]", 129),
	} {
		t.Run(name, func(t *testing.T) { assertRejected(t, ninjaJSONTestField(base, "unknown", value)) })
	}
	t.Run("trailing-direct", func(t *testing.T) { assertRejected(t, base+` {}`) })
}

func TestRecurringUniqueJSONEnvelopeAndPagination(t *testing.T) {
	data := string(recurringTestJSON(t, recurringTestRecord(t)))
	base := `{"data":` + data + `}`
	cases := map[string]string{
		"duplicate-data": ninjaJSONTestField(base, "data", data),
		"case-data":      ninjaJSONTestField(base, "DATA", data),
		"meta":           ninjaJSONTestField(base, "meta", `{"pagination":{},"PAGINATION":{}}`),
		"pagination":     ninjaJSONTestField(base, "meta", `{"pagination":{"total":0,"TOTAL":1}}`),
		"unicode":        ninjaJSONTestField(base, "meta", `{"pagination":{"unknown":[{"s":0,"ſ":1}]}}`),
		"envelope":       ninjaJSONTestField(ninjaJSONTestField(base, "meta", `{}`), "META", `{}`),
		"trailing":       base + ` {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			c, calls := recurringReaderServer(t, []byte(body), SupportedRecurringVersion)
			if _, err := c.Get(context.Background(), "recurring-A"); err == nil || calls.Load() != 1 {
				t.Fatal("ambiguous envelope accepted or retried")
			}
		})
	}
	page := string(recurringTestJSON(t, recurringTestPage([]RecurringRecord{recurringTestRecord(t)}, 1, 1, 1)))
	for _, alias := range ninjaJSONTestAliases("total_pages") {
		t.Run("find/"+alias, func(t *testing.T) {
			body := strings.Replace(page, `"total_pages":1`, fmt.Sprintf(`%q:2,"total_pages":1`, alias), 1)
			s := recurringTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Error("inventory attempted a write")
				}
				fmt.Fprint(w, body)
			})
			c := recurringTestClient(t, s.URL, 1, 1)
			if _, err := c.Find(context.Background(), "client-A"); !errors.Is(err, ErrLookupIncomplete) || !errors.Is(err, errInvalidResponse) {
				t.Fatal("ambiguous pagination was not incomplete", err)
			}
		})
	}
}

func TestRecurringUniqueJSONPreservesForeignHistory(t *testing.T) {
	expected := recurringTestRecord(t)
	expected.StatusID, expected.IsDeleted, expected.ArchivedAt = "2", true, 99
	expected.LastSentDate = "2040-01-01 08:00:00"
	expected.LineItems = append(expected.LineItems, expected.LineItems[0])
	expected.Discount = "2.123456789012345678901"
	body := ninjaJSONTestField(string(recurringTestJSON(t, expected)), "未知", `{"ſ":1,"K":2,"é":3,"e":4,"large":1e10000}`)
	var direct RecurringRecord
	if err := json.Unmarshal([]byte(body), &direct); err != nil || !reflect.DeepEqual(direct, expected) {
		t.Fatal("unique non-ASCII metadata or history rejected", err)
	}
	c, _ := recurringReaderServer(t, []byte(`{"data":`+body+`,"meta":{"未知":[{"é":1,"e":2}]}}`), SupportedRecurringVersion)
	if got, err := c.Get(context.Background(), expected.ID); err != nil || !reflect.DeepEqual(got, expected) {
		t.Fatal("reader changed valid history", err)
	}
}
