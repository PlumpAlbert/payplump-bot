package importverify

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/PlumpAlbert/payplump-bot/internal/invoiceninja"
)

type discoveryCustomers struct {
	CustomerReader
	rows []invoiceninja.Customer
	err  error
}

func (d discoveryCustomers) ListCustomers(context.Context) ([]invoiceninja.Customer, error) {
	return d.rows, d.err
}

type discoveryIdentityReader struct{ IdentityReader }

type discoveryOtherProviders struct {
	RecurringReader
	InvoiceReader
	PaymentReader
	RecordReader
	SettingsReader
}

func discoveryTestPolicy() Policy {
	return Policy{Automatic: true, BotID: 9001, SourceScopeHash: strings.Repeat("a", 64), NinjaVersion: "5.13.47", SUIVersion: "1.6.4", NativeCurrencyID: "1", NativeCurrencyCode: "USD", ProductKey: "legacy", PublicSubscriptionBase: "https://subscriptions.invalid/", ExpectedInboundIDs: []int64{1}, MaxRecords: 20, MaxInventoryRecords: 2, MaxPages: 2, MaxHistoricalAnchors: 2, EvidenceTTL: time.Minute}
}

func TestDiscoverRequiresCompleteCustomerInventory(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []invoiceninja.Customer
		err  error
	}{
		{name: "api failure", err: errors.New("private provider response")},
		{name: "truncated at bound", rows: []invoiceninja.Customer{{ID: "one"}, {ID: "two"}, {ID: "three"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers := Providers{Customers: discoveryCustomers{rows: tc.rows, err: tc.err}, Identities: discoveryIdentityReader{}, Recurring: discoveryOtherProviders{}, Invoices: discoveryOtherProviders{}, Payments: discoveryOtherProviders{}, Records: discoveryOtherProviders{}, Settings: discoveryOtherProviders{}}
			svc, err := New(discoveryTestPolicy(), providers)
			if err != nil {
				t.Fatal(err)
			}
			got, err := svc.Discover(context.Background(), time.Now(), nil)
			if err == nil || got != nil {
				t.Fatalf("partial inventory must fail closed: discoveries=%v err=%v", got, err)
			}
			if strings.Contains(err.Error(), "private provider response") {
				t.Fatalf("provider response leaked: %v", err)
			}
		})
	}
}

func TestDiscoverRequiresAutomaticPolicyAndInventoryCapability(t *testing.T) {
	p := discoveryTestPolicy()
	p.Automatic = false
	providers := Providers{Customers: discoveryCustomers{}, Identities: discoveryIdentityReader{}, Recurring: discoveryOtherProviders{}, Invoices: discoveryOtherProviders{}, Payments: discoveryOtherProviders{}, Records: discoveryOtherProviders{}, Settings: discoveryOtherProviders{}}
	svc, err := New(p, providers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discover(context.Background(), time.Now(), nil); err == nil {
		t.Fatal("manual verification policy unexpectedly enabled discovery")
	}
	p.Automatic = true
	providers.Customers = customerWithoutInventory{}
	svc, err = New(p, providers)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Discover(context.Background(), time.Now(), nil); err == nil {
		t.Fatal("legacy customer reader unexpectedly qualified as complete inventory")
	}
}

type customerWithoutInventory struct{ CustomerReader }

func (customerWithoutInventory) FindCustomers(context.Context, int64) ([]invoiceninja.Customer, error) {
	return nil, nil
}
func (customerWithoutInventory) ListRecurring(context.Context, string) ([]invoiceninja.RecurringRecord, error) {
	return nil, nil
}
