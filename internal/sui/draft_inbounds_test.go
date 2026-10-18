package sui

import "testing"

func TestCreateDraftInboundIDsReturnsCopy(t *testing.T) {
	draft, err := NewDisabledDraft([]int64{7, 8})
	if err != nil {
		t.Fatal("synthetic draft failed")
	}
	ids := draft.InboundIDs()
	if len(ids) != 2 || ids[0] != 7 || ids[1] != 8 {
		t.Fatal("inbound getter changed frozen policy")
	}
	ids[0] = 99
	again := draft.InboundIDs()
	if len(again) != 2 || again[0] != 7 || again[1] != 8 {
		t.Fatal("inbound getter exposes mutable draft policy")
	}
}
