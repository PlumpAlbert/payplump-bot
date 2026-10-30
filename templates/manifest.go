package templates

// manifest lists the only editable files and the only fields each may use.
var manifest = map[string][]string{
	"activation/complete_admin":             {"OperationID", "ActorID", "NextInvoiceDate"},
	"activation/complete_client":            {"NextInvoiceDate"},
	"activation/review_admin":               {"OperationID"},
	"broadcast/audience_limit":              {},
	"broadcast/cancelled":                   {},
	"broadcast/confirm_audience":            {"Count", "Token"},
	"broadcast/draft_changed":               {},
	"broadcast/draft_expired":               {},
	"broadcast/draft_missing":               {},
	"broadcast/failed":                      {},
	"broadcast/instructions":                {},
	"broadcast/invalid_body":                {},
	"broadcast/invalid_command":             {},
	"broadcast/missing":                     {},
	"broadcast/policy_changed":              {},
	"broadcast/preview":                     {"Body", "Token"},
	"broadcast/status":                      {"Pending", "Enqueued", "Sent", "Failed", "Skipped"},
	"broadcast/status_unconfirmed":          {},
	"broadcast/summary":                     {"JobID", "Enqueued", "Sent", "Failed", "Skipped"},
	"broadcast/unauthorized":                {},
	"clientmenu/access_deadline":            {"SentDate"},
	"clientmenu/access_denied":              {},
	"clientmenu/access_requested":           {},
	"clientmenu/actions":                    {},
	"clientmenu/configuration_unavailable":  {},
	"clientmenu/excluded":                   {},
	"clientmenu/home":                       {"StatusText"},
	"clientmenu/instructions":               {},
	"clientmenu/invoice":                    {"InvoiceNumber", "Amount", "Balance", "DueDate", "InvoiceURL"},
	"clientmenu/invoice_cancelled":          {},
	"clientmenu/invoice_draft":              {},
	"clientmenu/invoice_footer":             {},
	"clientmenu/invoice_paid":               {},
	"clientmenu/invoice_pending":            {},
	"clientmenu/invoice_unavailable":        {},
	"clientmenu/label_configuration":        {},
	"clientmenu/label_help":                 {},
	"clientmenu/label_invoice":              {},
	"clientmenu/label_status":               {},
	"clientmenu/next_invoice_date":          {"NextInvoiceDate"},
	"clientmenu/payment_bank":               {"PaymentBank"},
	"clientmenu/payment_date":               {"PaymentDate"},
	"clientmenu/payment_details":            {"Balance", "PaymentPhone"},
	"clientmenu/payment_details_missing":    {},
	"clientmenu/payment_recipient":          {"PaymentRecipient"},
	"clientmenu/payment_unverified":         {},
	"clientmenu/register_first":             {},
	"clientmenu/state_active":               {},
	"clientmenu/state_awaiting_payment":     {},
	"clientmenu/state_overdue":              {},
	"clientmenu/state_payment_review":       {},
	"clientmenu/state_setting_up":           {},
	"clientmenu/state_unknown":              {},
	"clientmenu/status":                     {"State"},
	"clientmenu/status_footer":              {},
	"clientmenu/welcome":                    {},
	"configdelivery/denied":                 {},
	"configdelivery/subscription":           {"SubscriptionURL"},
	"import/active_period_expired":          {},
	"import/admission_conflict":             {},
	"import/ambiguous_source":               {},
	"import/excluded":                       {},
	"import/historical_anchor_limit":        {},
	"import/invalid_policy":                 {},
	"import/issue_admin":                    {"Subject", "Reason", "DiagnosticCode"},
	"import/paused_history_not_supported":   {},
	"import/payment_unverified":             {},
	"import/registration_blocked":           {},
	"import/source_changed":                 {},
	"import/source_failure":                 {},
	"import/subject_actor":                  {"ActorID"},
	"import/subject_candidate":              {"DiagnosticCode"},
	"import/subject_general":                {},
	"import/unpaid_debt":                    {},
	"import/unsupported_profile":            {},
	"lifecycle/invoice_admin":               {"Status", "ActorID", "InvoiceNumber", "Balance", "SentDate"},
	"lifecycle/invoice_client":              {"Verb", "InvoiceNumber", "Balance", "SentDate"},
	"lifecycle/invoice_issued":              {},
	"lifecycle/invoice_updated":             {},
	"lifecycle/overdue_admin":               {"ActorID", "InvoiceNumber"},
	"lifecycle/overdue_client":              {"InvoiceNumber", "SentDate"},
	"lifecycle/overdue_review_admin":        {"ActorID", "InvoiceNumber", "ReviewCode", "ConfirmedSteps"},
	"lifecycle/renewal_admin":               {"ActorID", "InvoiceNumber", "Amount", "CurrencyID", "PaymentDate", "TargetDate"},
	"lifecycle/renewal_client":              {"InvoiceNumber", "TargetDate"},
	"lifecycle/renewal_review_admin":        {"ActorID", "ClientID", "InvoiceID", "ReviewCode"},
	"lifecycle/review_admin":                {"ActorID", "InvoiceNumber", "Reason"},
	"lifecycle/review_association":          {},
	"lifecycle/review_balance":              {},
	"lifecycle/review_collision":            {},
	"lifecycle/review_contract":             {},
	"lifecycle/review_excluded":             {},
	"lifecycle/review_invoice":              {},
	"lifecycle/review_previous_payment":     {},
	"lifecycle/review_sent_date":            {},
	"lifecycle/unknown_invoice":             {},
	"payments/confirmed_admin":              {"ActorID", "InvoiceNumber", "Amount", "PaymentDate", "NextInvoiceDate"},
	"payments/confirmed_client":             {"InvoiceNumber", "Amount", "PaymentDate", "NextInvoiceDate"},
	"payments/review_admin":                 {"ActorID", "ClientID"},
	"registration/default_name":             {},
	"registration/invoice":                  {"InvoiceNumber", "Amount", "DueDate", "PaymentPhone"},
	"registration/invoice_admin":            {"DisplayName", "ActorID", "InvoiceNumber", "Amount", "DueDate", "InvoiceURL"},
	"registration/invoice_footer":           {"InvoiceURL"},
	"registration/payment_bank":             {"PaymentBank"},
	"registration/payment_recipient":        {"PaymentRecipient"},
	"registration/review_admin":             {"OperationID", "ReviewCode", "ActorID", "Reason"},
	"registration/review_client":            {},
	"registration/review_contract":          {},
	"registration/review_customer_conflict": {},
	"registration/review_default":           {},
	"registration/review_excluded":          {},
	"registration/review_existing_customer": {},
	"registration/review_invoice_conflict":  {},
	"registration/review_invoice_state":     {},
	"registration/review_local_association": {},
	"registration/review_unknown_write":     {},
}

// Data contains only explicitly selected display strings, never domain objects.
type Data struct {
	ActorID          string
	Amount           string
	Balance          string
	Body             string
	ClientID         string
	ConfirmedSteps   string
	Count            string
	CurrencyID       string
	DiagnosticCode   string
	DisplayName      string
	DueDate          string
	Enqueued         string
	Failed           string
	InvoiceID        string
	InvoiceNumber    string
	InvoiceURL       string
	JobID            string
	NextInvoiceDate  string
	OperationID      string
	PaymentBank      string
	PaymentDate      string
	PaymentPhone     string
	PaymentRecipient string
	Pending          string
	Reason           string
	ReviewCode       string
	Sent             string
	SentDate         string
	Skipped          string
	State            string
	Status           string
	StatusText       string
	Subject          string
	SubscriptionURL  string
	TargetDate       string
	Token            string
	Verb             string
}
