package protocol

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestWriteLoadRoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		status Status
	}{
		{
			name: "full awaiting review",
			status: Status{
				UpdatedAt:      time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC),
				Task:           "eos#42",
				Branch:         "feat-eos-env-42",
				Phase:          PhaseAwaitingReview,
				RealWorldProof: "ran on orb debian, systemctl status green",
				FilesTouched:   []string{"cmd/env.go", "internal/config/config.go"},
				Tests: []TestRun{
					{Cmd: "make test", Target: "./internal/...", Result: ResultPass},
					{Cmd: "make lint", Target: "./...", Result: ResultPass},
					{Cmd: "make test", Target: "./internal/gate", Result: ResultFail, ExpectedResult: ResultFail},
				},
				DiffStat: DiffStat{Files: 2, Insertions: 88, Deletions: 4},
			},
		},
		{
			name: "blocked minimal",
			status: Status{
				UpdatedAt:     time.Date(2026, 7, 18, 13, 30, 0, 0, time.UTC),
				Task:          "themis#7",
				Branch:        "fix-themis-7",
				Phase:         PhaseBlocked,
				BlockedReason: "needs a decision on prod config path",
			},
		},
		{
			name:   "zero value",
			status: Status{},
		},
		{
			name: "blocked with question and answer",
			status: Status{
				UpdatedAt:     time.Date(2026, 7, 18, 14, 0, 0, 0, time.UTC),
				Task:          "themis#8",
				Branch:        "fix-themis-8",
				Phase:         PhaseBlocked,
				BlockedReason: "guard under test doesn't exist yet on base",
				Question: &Question{
					Text:    "wait for the other branch to merge and rebase, or pull the commit in now?",
					Options: []string{"wait and rebase", "cherry-pick now"},
				},
				Answer: &Answer{
					Text:       "cherry-pick now",
					Option:     2,
					AnsweredAt: time.Date(2026, 7, 18, 14, 5, 0, 0, time.UTC),
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := StatusPath(t.TempDir())
			if err := Write(path, &tc.status); err != nil {
				t.Fatalf("Write: %v", err)
			}
			got, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if !got.UpdatedAt.Equal(tc.status.UpdatedAt) {
				t.Errorf("UpdatedAt: got %v want %v", got.UpdatedAt, tc.status.UpdatedAt)
			}
			// Compare the rest via JSON so slice/struct equality is structural.
			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(tc.status)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("round trip mismatch:\n got %s\nwant %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestLoadMissingFileIsNotExist(t *testing.T) {
	_, err := Load(StatusPath(t.TempDir()))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("want os.ErrNotExist, got %v", err)
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := StatusPath(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("want error decoding malformed status file, got nil")
	}
}

func TestWriteUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) // let t.TempDir's own cleanup remove it

	path := filepath.Join(dir, "sub", "status.json")
	if err := Write(path, &Status{Task: "x", Phase: PhaseWorking}); err == nil {
		t.Fatal("want error writing status under a read-only parent, got nil")
	}
}

func TestWriteLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := StatusPath(dir)
	if err := Write(path, &Status{Task: "x", Phase: PhaseWorking}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "status.json" {
			t.Errorf("unexpected leftover file after Write: %s", e.Name())
		}
	}
}

// TestWriteIsAtomicUnderConcurrentReads hammers Write while readers race to
// Load. Because Write renames a fully-written temp file into place, every read
// must see either "not yet written" or a completely valid Status — never a
// half-written, unparseable file. A non-atomic writer (truncate-in-place) would
// intermittently fail the json decode here.
func TestWriteIsAtomicUnderConcurrentReads(t *testing.T) {
	path := StatusPath(t.TempDir())

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers.
	for w := range 4 {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range 200 {
				s := Status{
					UpdatedAt:    time.Now(),
					Task:         "race",
					Phase:        PhaseWorking,
					FilesTouched: make([]string, id+i%7), // vary size so writes differ in length
				}
				if err := Write(path, &s); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
				select {
				case <-stop:
					return
				default:
				}
			}
		}(w)
	}

	// Readers: every successful read must parse; a not-exist early on is fine.
	for range 4 {
		wg.Go(func() {
			for range 500 {
				_, err := Load(path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Load saw a partial/invalid file: %v", err)
					return
				}
			}
		})
	}

	wg.Wait()
	close(stop)
}

func TestSelfReportEqual(t *testing.T) {
	base := func() Status {
		return Status{
			Title:          "fix: narrow the claimed test run",
			RealWorldProof: "ran the two commands directly",
			DiffStat:       DiffStat{Files: 1, Insertions: 2, Deletions: 1},
			FilesTouched:   []string{"a.go", "b.go"},
			Plan:           []string{"narrow tests", "verify"},
			Tests:          []TestRun{{Cmd: "go test ./...", Result: ResultPass}},
			// Fields SelfReportEqual deliberately ignores: a diff here must never
			// flip the result, since these are argus's own bookkeeping, not the
			// worker's report content.
			UpdatedAt:     time.Now(),
			Phase:         PhaseAwaitingReview,
			BlockedReason: "stuck",
			Task:          "issue-369",
			Branch:        "argus-fix-issue-369",
			PRURL:         "https://example.com/pr/1",
		}
	}

	cases := []struct {
		modify func(*Status)
		name   string
		want   bool
	}{
		{modify: func(s *Status) {}, name: "identical reports", want: true},
		{modify: func(s *Status) { s.Title = "fix: something else" }, name: "differing title", want: false},
		{modify: func(s *Status) { s.RealWorldProof = "" }, name: "differing real world proof", want: false},
		{modify: func(s *Status) { s.DiffStat.Insertions++ }, name: "differing diff stat", want: false},
		{modify: func(s *Status) { s.FilesTouched = append(s.FilesTouched, "c.go") }, name: "differing files touched", want: false},
		{modify: func(s *Status) { s.Plan = []string{"different plan"} }, name: "differing plan", want: false},
		{
			modify: func(s *Status) {
				s.Tests = []TestRun{{Cmd: "go test ./... -race -count=2", Result: ResultPass}}
			},
			name: "differing tests",
			want: false,
		},
		{
			name: "argus-owned fields differ but self-report is identical",
			modify: func(s *Status) {
				s.UpdatedAt = s.UpdatedAt.Add(time.Hour)
				s.Phase = PhaseWorking
				s.BlockedReason = ""
				s.Task = "different-task"
				s.Branch = "different-branch"
				s.PRURL = ""
			},
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := base()
			b := base()
			tc.modify(&b)
			if got := SelfReportEqual(&a, &b); got != tc.want {
				t.Errorf("SelfReportEqual(a, b) = %v, want %v (a=%+v, b=%+v)", got, tc.want, a, b)
			}
			if got := SelfReportEqual(&b, &a); got != tc.want {
				t.Errorf("SelfReportEqual is not symmetric: SelfReportEqual(b, a) = %v, want %v", got, tc.want)
			}
		})
	}
}
