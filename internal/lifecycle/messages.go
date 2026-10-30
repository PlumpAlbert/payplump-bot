package lifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/PlumpAlbert/payplump-bot/internal/queue"
	"github.com/PlumpAlbert/payplump-bot/templates"
)

func makeNotices(botID int64, admins []int64, snapshot Snapshot, invoice Invoice, reviewCode int) Notices {
	identity := invoice.ID
	if identity == "" {
		identity = fmt.Sprintf("client:%d", snapshot.ClientID)
	}
	digest := sha256.Sum256([]byte(identity))
	prefix := "invoice-lifecycle:" + hex.EncodeToString(digest[:])
	if reviewCode != 0 {
		reason := reviewText(reviewCode)
		number := invoice.Number
		if !printable(number, 128) {
			number = templates.Render("lifecycle/unknown_invoice", templates.Data{})
		}
		body := templates.Render("lifecycle/review_admin", templates.Data{ActorID: strconv.FormatInt(snapshot.TelegramID, 10), InvoiceNumber: number, Reason: reason})
		out := Notices{}
		for _, id := range sortedIDs(admins) {
			out.Review = append(out.Review, queue.Draft{BotID: botID, ChatID: id, Key: prefix + ":review:admin:" + strconv.FormatInt(id, 10), Text: body})
		}
		return out
	}
	if invoice.Status != "sent" && invoice.Status != "partial" {
		return Notices{}
	}
	verb := templates.Render("lifecycle/invoice_issued", templates.Data{})
	if invoice.Status == "partial" {
		verb = templates.Render("lifecycle/invoice_updated", templates.Data{})
	}
	body := templates.Render("lifecycle/invoice_client", templates.Data{Verb: verb, InvoiceNumber: invoice.Number, Balance: invoice.Balance, SentDate: invoice.SentDate})
	out := Notices{Client: &queue.Draft{BotID: botID, ChatID: snapshot.ChatID, Key: prefix + ":issued:client", Text: body}}
	for _, id := range sortedIDs(admins) {
		out.Admins = append(out.Admins, queue.Draft{BotID: botID, ChatID: id, Key: prefix + ":issued:admin:" + strconv.FormatInt(id, 10), Text: templates.Render("lifecycle/invoice_admin", templates.Data{Status: invoice.Status, ActorID: strconv.FormatInt(snapshot.TelegramID, 10), InvoiceNumber: invoice.Number, Balance: invoice.Balance, SentDate: invoice.SentDate})})
	}
	return out
}

func reviewText(code int) string {
	switch code {
	case ReviewAssociation:
		return templates.Render("lifecycle/review_association", templates.Data{})
	case ReviewInvoice:
		return templates.Render("lifecycle/review_invoice", templates.Data{})
	case ReviewSentDate:
		return templates.Render("lifecycle/review_sent_date", templates.Data{})
	case ReviewBalance:
		return templates.Render("lifecycle/review_balance", templates.Data{})
	case ReviewCollision:
		return templates.Render("lifecycle/review_collision", templates.Data{})
	case ReviewExcluded:
		return templates.Render("lifecycle/review_excluded", templates.Data{})
	case ReviewPreviousPayment:
		return templates.Render("lifecycle/review_previous_payment", templates.Data{})
	default:
		return templates.Render("lifecycle/review_contract", templates.Data{})
	}
}

func sortedIDs(ids []int64) []int64 {
	out := append([]int64(nil), ids...)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func printable(value string, max int) bool {
	return utf8.ValidString(value) && strings.TrimSpace(value) == value && utf8.RuneCountInString(value) <= max && strings.IndexFunc(value, unicode.IsControl) < 0
}
