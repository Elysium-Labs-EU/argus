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
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 500 {
				_, err := Load(path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Errorf("Load saw a partial/invalid file: %v", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(stop)
}
