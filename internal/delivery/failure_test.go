package delivery

import (
	"testing"

	"github.com/orpheus-agents/orpheus-mattermost/internal/conversation"
)

func TestFailureQuotesErrorMessage(t *testing.T) {
	for _, tc := range []struct {
		name, status, stop, message, want string
	}{
		{"capacity", "failed", "", "Selected model is at capacity. Please try a different model.", "Run failed (harness_failed).\n\n> Selected model is at capacity. Please try a different model."},
		{"multiline", "failed", "", "Ошибка модели\n\nTry again.", "Run failed (harness_failed).\n\n> Ошибка модели\n> \n> Try again."},
		{"line endings", "failed", "", " First\r\nSecond\rThird\n ", "Run failed (harness_failed).\n\n> First\n> Second\n> Third"},
		{"empty", "failed", "", "", "Run failed (harness_failed)."},
		{"whitespace", "failed", "", " \t\n", "Run failed (harness_failed)."},
		{"cancelled", "cancelled", "", "ignored", "Run cancelled."},
		{"token limit", "cancelled", "token_limit", "ignored", "Token limit reached. Start a new thread to continue."},
		{"completed", "completed", "", "ignored", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := conversation.Run{Status: tc.status, StopReason: tc.stop, Error: "harness_failed", ErrorMessage: tc.message}
			if got := Failure(run, conversation.Render{Version: 4}); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	for version := 1; version <= 3; version++ {
		run := conversation.Run{Status: "failed", Error: "harness_failed", ErrorMessage: "Selected model is at capacity."}
		if got := Failure(run, conversation.Render{Version: version}); got != "Run failed (harness_failed)." {
			t.Fatalf("renderer %d changed persisted notice: %q", version, got)
		}
	}
}
