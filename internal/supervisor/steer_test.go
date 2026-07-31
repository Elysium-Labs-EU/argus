package supervisor

import "testing"

// TestSteerMessage pins the exact rendered text so a future edit can't
// silently change what a worker sees mid-turn without a test noticing.
func TestSteerMessage(t *testing.T) {
	got := SteerMessage("double-check the retry backoff caps at 30s, not 30ms")
	want := "The supervisor sent a follow-up while you're still working — this is not a new task.\n\n" +
		"Note: double-check the retry backoff caps at 30s, not 30ms\n\n" +
		"Keep your existing plan and context; fold this into your current turn instead of restarting. Continue working and report your next phase via `argus worker report <phase>` as normal.\n"
	if got != want {
		t.Errorf("SteerMessage() =\n%q\nwant\n%q", got, want)
	}
}
