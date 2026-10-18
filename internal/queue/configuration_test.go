package queue

import "testing"

func TestProtectedConfigurationDraftContainsOnlyTypedMarker(t *testing.T) {
	valid := Draft{BotID: 7, ChatID: 101, Key: "synthetic-config", Text: ConfigurationIntentBody, ProtectedConfiguration: true}
	if valid.Validate() != nil {
		t.Fatal("valid protected intent rejected")
	}
	for _, change := range []func(*Draft){func(d *Draft) { d.Text = "https://subscription.example.test/private?format=json" }, func(d *Draft) { d.Text = "" }, func(d *Draft) { d.ClientMenu = true }} {
		d := valid
		change(&d)
		if d.Validate() == nil {
			t.Fatal("secret-bearing or ambiguous protected draft accepted")
		}
	}
}
