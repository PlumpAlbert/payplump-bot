package queue

import (
	"strings"
	"testing"
)

func TestDraftValidation(t *testing.T) {
	valid := Draft{BotID: 123, ChatID: 42, Key: "invoice:7:created", Text: "Оплатите счёт"}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Draft){
		func(d *Draft) { d.BotID = 0 },
		func(d *Draft) { d.ChatID = -1 },
		func(d *Draft) { d.Key = "" },
		func(d *Draft) { d.Key = strings.Repeat("x", 257) },
		func(d *Draft) { d.Key = "nul\x00key" },
		func(d *Draft) { d.Text = "" },
		func(d *Draft) { d.Text = string([]byte{0xff}) },
		func(d *Draft) { d.Text = "nul\x00text" },
		func(d *Draft) { d.Text = strings.Repeat("я", 4097) },
	} {
		draft := valid
		change(&draft)
		if err := draft.Validate(); err == nil || strings.Contains(err.Error(), draft.Text) && draft.Text != "" {
			t.Fatal("invalid draft accepted or message contents leaked")
		}
	}
}
