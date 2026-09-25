package conversation

import (
	"encoding/json"
	"testing"
)

func TestDecodeMetadataContract(t *testing.T) {
	e := Envelope{Schema: 1, Source: "chat", Workflow: "assistant", Channel: "channel", Root: "root", Anchor: "anchor", Kind: "initial", TriggerIDs: []string{"anchor"}, Render: Render{Version: 2, MaxChars: 12000}}
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(raw)
	if err != nil || got.Anchor != e.Anchor || got.Render != e.Render {
		t.Fatalf("metadata lost: %+v %v", got, err)
	}
	for _, raw := range []json.RawMessage{nil, []byte("null"), []byte("[]"), []byte(`{"schema":1}`)} {
		if _, err := Decode(raw); err == nil {
			t.Fatalf("invalid metadata accepted: %s", raw)
		}
	}
}
