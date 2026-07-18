package codeberg

import (
	"strings"
	"testing"
)

func TestApiMessagePrefersMessageField(t *testing.T) {
	if got := apiMessage([]byte(`{"message":"branch already exists"}`)); got != "branch already exists" {
		t.Errorf("apiMessage: got %q", got)
	}
}

func TestApiMessageFallsBackToBody(t *testing.T) {
	got := apiMessage([]byte("not json"))
	if got != "not json" {
		t.Errorf("apiMessage fallback: got %q", got)
	}
}

func TestApiMessageTruncatesLongBody(t *testing.T) {
	long := strings.Repeat("x", 500)
	if got := apiMessage([]byte(long)); len(got) != 300 {
		t.Errorf("apiMessage should truncate to 300, got %d", len(got))
	}
}

func TestNewSetsToken(t *testing.T) {
	c := New("tok")
	if c.token != "tok" || c.http == nil {
		t.Errorf("New did not initialize client: %+v", c)
	}
}
