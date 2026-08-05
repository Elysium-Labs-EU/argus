package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Elysium-Labs-EU/argus/internal/ownership"
	"github.com/Elysium-Labs-EU/argus/internal/protocol"
)

func TestClassifyAbsentWhenLoaderReportsErrNotExist(t *testing.T) {
	status, msg := classify("/nonexistent/status.json", true, fmt.Errorf("reading status file: %w", os.ErrNotExist))
	if status != FileAbsent || msg != "" {
		t.Errorf("classify = (%q, %q), want (absent, \"\")", status, msg)
	}
}

func TestClassifyAbsentWhenFoundFalse(t *testing.T) {
	status, msg := classify("/nonexistent/verdict.json", false, nil)
	if status != FileAbsent || msg != "" {
		t.Errorf("classify = (%q, %q), want (absent, \"\")", status, msg)
	}
}

func TestClassifyUnreadableOnDecodeError(t *testing.T) {
	status, msg := classify("/some/path.json", true, errors.New("decoding status file: unexpected EOF"))
	if status != FileUnreadable || msg == "" {
		t.Errorf("classify = (%q, %q), want (unreadable, non-empty)", status, msg)
	}
}

func TestClassifyUnreadableOnNullJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, msg := classify(path, true, nil)
	if status != FileUnreadable || msg == "" {
		t.Errorf("classify = (%q, %q), want (unreadable, non-empty) for a literal null file", status, msg)
	}
}

func TestClassifyOKOnValidContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "status.json")
	if err := os.WriteFile(path, []byte(`{"phase":"planning"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	status, msg := classify(path, true, nil)
	if status != FileOK || msg != "" {
		t.Errorf("classify = (%q, %q), want (ok, \"\")", status, msg)
	}
}

func TestIsNullJSON(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    bool
	}{
		{"literal null", "null", true},
		{"null with trailing whitespace", "null\n", true},
		{"empty object", "{}", false},
		{"real content", `{"phase":"planning"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(dir, c.name+".json")
			if err := os.WriteFile(path, []byte(c.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := isNullJSON(path); got != c.want {
				t.Errorf("isNullJSON(%q) = %v, want %v", c.content, got, c.want)
			}
		})
	}
}

func TestIsNullJSONMissingFileIsFalse(t *testing.T) {
	if isNullJSON(filepath.Join(t.TempDir(), "missing.json")) {
		t.Error("isNullJSON on a missing file should be false — that's classify's FileAbsent case, not unreadable")
	}
}

// initFleetRepo builds one real repo with a linked worktree per branch in
// branches, so ListLinkedWorktrees exercises real git plumbing exactly like
// prune's own tests do. It returns repoRoot plus a branch->worktree map so a
// caller can seed each worktree's control-plane files independently before
// calling BuildFleet.
func initFleetRepo(t *testing.T, branches ...string) (repoRoot string, worktrees map[string]string) {
	t.Helper()
	repoRoot, base := initGitRepo(t)
	worktrees = make(map[string]string, len(branches))
	for _, branch := range branches {
		gitDo(t, repoRoot, "checkout", "-q", "-b", branch, "origin/"+base)
		gitDo(t, repoRoot, "checkout", "-q", base) // repoRoot can't keep branch checked out too — worktree add below needs it free
		wt := filepath.Join(t.TempDir(), branch)
		gitDo(t, repoRoot, "worktree", "add", "-q", wt, branch)
		worktrees[branch] = wt
	}
	return repoRoot, worktrees
}

// TestBuildFleetAcrossAssortedWorktreeStates covers the states the brief
// calls out by name: an in-flight worker (planning), one waiting on review
// with an approving verdict, one whose PR already merged, one with a
// syntactically corrupt status.json, one whose status.json is the literal
// JSON value "null" (the fail-open case protocol.Load doesn't guard), and
// one with no owner.json at all.
func TestBuildFleetAcrossAssortedWorktreeStates(t *testing.T) {
	repoRoot, worktrees := initFleetRepo(t,
		"planning", "awaiting-review", "merged", "corrupt-status", "null-status", "no-owner")
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	planningWT := worktrees["planning"]
	if err := protocol.Write(protocol.StatusPath(planningWT), &protocol.Status{Phase: protocol.PhasePlanning, Title: "wip"}); err != nil {
		t.Fatal(err)
	}

	reviewWT := worktrees["awaiting-review"]
	if err := protocol.Write(protocol.StatusPath(reviewWT), &protocol.Status{Phase: protocol.PhaseAwaitingReview, Title: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := protocol.WriteApproval(reviewWT, &protocol.Approval{Approved: true, Source: "gate"}); err != nil {
		t.Fatal(err)
	}
	heartbeat := now.Add(-2 * time.Minute)
	if err := ownership.Write(reviewWT, &ownership.Owner{OwnerID: "sess-1", OwnerLabel: "host (pid 1)", SpawnedAt: heartbeat, HeartbeatAt: heartbeat}); err != nil {
		t.Fatal(err)
	}

	mergedWT := worktrees["merged"]
	if err := protocol.WriteLifecycle(mergedWT, &protocol.Lifecycle{State: protocol.LifecycleMerged, PRNumber: 42, PRURL: "https://example.com/pr/42"}); err != nil {
		t.Fatal(err)
	}

	corruptWT := worktrees["corrupt-status"]
	if err := os.MkdirAll(filepath.Dir(protocol.StatusPath(corruptWT)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protocol.StatusPath(corruptWT), []byte("{not valid json"), 0o600); err != nil {
		t.Fatal(err)
	}

	nullWT := worktrees["null-status"]
	if err := os.MkdirAll(filepath.Dir(protocol.StatusPath(nullWT)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(protocol.StatusPath(nullWT), []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}

	noOwnerWT := worktrees["no-owner"]
	if err := protocol.Write(protocol.StatusPath(noOwnerWT), &protocol.Status{Phase: protocol.PhaseWorking}); err != nil {
		t.Fatal(err)
	}

	rows, err := BuildFleet(ctx, repoRoot, now)
	if err != nil {
		t.Fatalf("BuildFleet: %v", err)
	}
	if len(rows) != 6 {
		t.Fatalf("want 6 rows, got %d: %+v", len(rows), rows)
	}

	byBranch := make(map[string]FleetRow, len(rows))
	for i := range rows {
		byBranch[rows[i].Branch] = rows[i]
	}

	if r := byBranch["planning"]; r.StatusFile != FileOK || r.Status.Phase != protocol.PhasePlanning {
		t.Errorf("planning: status file=%q phase=%q, want ok/planning", r.StatusFile, r.Status.Phase)
	} else if r.VerdictFile != FileAbsent || r.LifecycleFile != FileAbsent || r.OwnerFile != FileAbsent {
		t.Errorf("planning: want verdict/lifecycle/owner all absent, got %+v", r)
	}

	if r := byBranch["awaiting-review"]; r.StatusFile != FileOK || r.Status.Phase != protocol.PhaseAwaitingReview {
		t.Errorf("awaiting-review: status = %+v", r)
	} else if r.VerdictFile != FileOK || !r.Verdict.Approved {
		t.Errorf("awaiting-review: verdict = %+v", r)
	} else if r.OwnerFile != FileOK || r.HeartbeatAge != 2*time.Minute {
		t.Errorf("awaiting-review: owner file=%q heartbeat age=%v, want ok/2m", r.OwnerFile, r.HeartbeatAge)
	}

	if r := byBranch["merged"]; r.StatusFile != FileAbsent {
		t.Errorf("merged: want status absent (worker never reported after ship), got %q", r.StatusFile)
	} else if r.LifecycleFile != FileOK || r.Lifecycle.State != protocol.LifecycleMerged || r.Lifecycle.PRNumber != 42 {
		t.Errorf("merged: lifecycle = %+v", r)
	}

	if r := byBranch["corrupt-status"]; r.StatusFile != FileUnreadable || r.StatusErr == "" {
		t.Errorf("corrupt-status: status file=%q err=%q, want unreadable with a message", r.StatusFile, r.StatusErr)
	}

	if r := byBranch["null-status"]; r.StatusFile != FileUnreadable || r.StatusErr == "" {
		t.Errorf("null-status: status file=%q err=%q, want unreadable (fail-open null must not look absent or ok)", r.StatusFile, r.StatusErr)
	}

	if r := byBranch["no-owner"]; r.OwnerFile != FileAbsent || r.HeartbeatAge != 0 {
		t.Errorf("no-owner: owner file=%q heartbeat age=%v, want absent/0 (never computed off a missing lease)", r.OwnerFile, r.HeartbeatAge)
	}
}

func TestFleetRowForeign(t *testing.T) {
	mine := &FleetRow{OwnerFile: FileOK, Owner: ownership.Owner{OwnerID: "me"}}
	if mine.Foreign("me") {
		t.Error("a matching owner_id should not be foreign")
	}
	theirs := &FleetRow{OwnerFile: FileOK, Owner: ownership.Owner{OwnerID: "them"}}
	if !theirs.Foreign("me") {
		t.Error("a mismatched owner_id should be foreign")
	}
	unowned := &FleetRow{OwnerFile: FileAbsent}
	if unowned.Foreign("me") {
		t.Error("no owner.json at all should not be foreign — it can't be attributed away")
	}
	unreadable := &FleetRow{OwnerFile: FileUnreadable}
	if unreadable.Foreign("me") {
		t.Error("a corrupt owner.json should not be foreign — it must stay visible, not be silently attributed away")
	}
}

func TestFleetRowIdle(t *testing.T) {
	if (&FleetRow{StatusFile: FileAbsent}).Idle() != true {
		t.Error("no status.json at all should be idle")
	}
	if (&FleetRow{StatusFile: FileUnreadable}).Idle() != false {
		t.Error("an unreadable status.json is a real anomaly, not idle")
	}
	if (&FleetRow{StatusFile: FileOK}).Idle() != false {
		t.Error("a real status.json should not be idle")
	}
}

func TestFilterFleet(t *testing.T) {
	rows := []FleetRow{
		{Branch: "mine", StatusFile: FileOK, OwnerFile: FileOK, Owner: ownership.Owner{OwnerID: "me"}},
		{Branch: "unowned", StatusFile: FileOK, OwnerFile: FileAbsent},
		{Branch: "foreign", StatusFile: FileOK, OwnerFile: FileOK, Owner: ownership.Owner{OwnerID: "them"}},
		{Branch: "idle-mine", StatusFile: FileAbsent, OwnerFile: FileOK, Owner: ownership.Owner{OwnerID: "me"}},
	}

	defaultScope := FilterFleet(rows, "me", false, false)
	byBranch := make(map[string]bool, len(defaultScope.Rows))
	for _, r := range defaultScope.Rows {
		byBranch[r.Branch] = true
	}
	if !byBranch["mine"] || !byBranch["unowned"] || byBranch["foreign"] || byBranch["idle-mine"] {
		t.Errorf("default scope = %+v, want mine+unowned (unowned can't be attributed away) but not foreign or idle", defaultScope.Rows)
	}
	if defaultScope.ExcludedForeignCount != 1 {
		t.Errorf("excluded foreign count = %d, want 1", defaultScope.ExcludedForeignCount)
	}
	if defaultScope.ExcludedIdleCount != 1 {
		t.Errorf("excluded idle count = %d, want 1 (idle-mine only — the foreign row is excluded as foreign, never double-counted as idle too)", defaultScope.ExcludedIdleCount)
	}

	withIdle := FilterFleet(rows, "me", false, true)
	branches := make(map[string]bool, len(withIdle.Rows))
	for _, r := range withIdle.Rows {
		branches[r.Branch] = true
	}
	if !branches["mine"] || !branches["unowned"] || branches["foreign"] || !branches["idle-mine"] {
		t.Errorf("--include-idle without --all = %+v, want mine+unowned+idle-mine but not foreign", withIdle.Rows)
	}

	all := FilterFleet(rows, "me", true, true)
	if len(all.Rows) != len(rows) || all.ExcludedForeignCount != 0 || all.ExcludedIdleCount != 0 {
		t.Errorf("--all --include-idle should keep everything with zero exclusions: %+v", all)
	}
}

func TestFilterFleetForeignRowNeverDoubleCountedAsIdle(t *testing.T) {
	rows := []FleetRow{
		{Branch: "foreign-idle", StatusFile: FileAbsent, OwnerFile: FileOK, Owner: ownership.Owner{OwnerID: "them"}},
	}
	result := FilterFleet(rows, "me", false, false)
	if result.ExcludedForeignCount != 1 || result.ExcludedIdleCount != 0 {
		t.Errorf("a foreign+idle row should count once as foreign, not also as idle: %+v", result)
	}
}

func TestTicketKey(t *testing.T) {
	cases := []struct {
		s    string
		want string
	}{
		{"AP-1169-fix-thing", "AP-1169"},
		{"ap-1166-fix-thing", "AP-1166"},
		{"argus-fix-issue-554", ""},
		{"", ""},
		{"AP-1169", "AP-1169"},
		{"AP-1207: Fix DELETE /api/web_app/<uuid> 500", "AP-1207"},
		{"ap-1207: fix delete", "AP-1207"},
		{"nodash", ""},
		{"AP-12-34-fix-thing", "AP-12"},
	}
	for _, c := range cases {
		if got := TicketKey(c.s); got != c.want {
			t.Errorf("TicketKey(%q) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestFleetRowMarshalJSONNullsZeroTimestamps(t *testing.T) {
	row := FleetRow{Branch: "AP-1169-fix", StatusFile: FileOK}
	data, err := json.Marshal(&row)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["phase_updated_at"] != nil {
		t.Errorf("phase_updated_at should be omitted for a zero-value timestamp, got %v", decoded["phase_updated_at"])
	}
	status, ok := decoded["Status"].(map[string]any)
	if !ok {
		t.Fatalf("Status not decoded as an object: %v", decoded["Status"])
	}
	if _, present := status["updated_at"]; present {
		t.Errorf("Status.updated_at should be omitted for a zero value, got %v", status["updated_at"])
	}
	if decoded["ticket"] != "AP-1169" {
		t.Errorf("ticket = %v, want AP-1169", decoded["ticket"])
	}
	if bytes.Contains(data, []byte("0001-01-01")) {
		t.Errorf("encoded JSON should never contain the zero-time sentinel:\n%s", data)
	}
}

func TestFleetRowMarshalJSONKeepsRealTimestamps(t *testing.T) {
	when := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	row := FleetRow{
		Branch:     "feat-x",
		StatusFile: FileOK, Status: protocol.Status{UpdatedAt: when},
	}
	data, err := json.Marshal(&row)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded["phase_updated_at"] != when.Format(time.RFC3339) {
		t.Errorf("phase_updated_at = %v, want %s", decoded["phase_updated_at"], when.Format(time.RFC3339))
	}
}

func TestBuildFleetPropagatesListLinkedWorktreesError(t *testing.T) {
	_, err := BuildFleet(context.Background(), t.TempDir(), time.Now())
	if err == nil {
		t.Error("BuildFleet against a non-git directory should propagate ListLinkedWorktrees' error")
	}
}

func TestBuildFleetEmptyRepoNoLinkedWorktrees(t *testing.T) {
	repoRoot, _ := initGitRepo(t)
	rows, err := BuildFleet(context.Background(), repoRoot, time.Now())
	if err != nil {
		t.Fatalf("BuildFleet: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("want 0 rows for a repo with no linked worktrees, got %d", len(rows))
	}
}
