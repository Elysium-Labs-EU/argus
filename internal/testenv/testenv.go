// Package testenv scrubs process environment state that git-fixture tests
// must not inherit.
package testenv

import "os"

// gitHookEnvVars are exported by git into every hook's environment so the
// hook's own git invocations resolve back to the repo/worktree that
// triggered it (see githooks(5)). If a hook execs `go test` without
// clearing them, every subprocess the test binary spawns inherits them too
// — and an inherited GIT_DIR silently overrides an explicit `git -C <dir>`,
// so a test fixture's "isolated" tempdir repo is ignored in favor of the
// real one.
var gitHookEnvVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_PREFIX",
	"GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
}

// ScrubGitHookEnv unsets git hook environment variables inherited from a
// parent git process. Call it once from TestMain, before any git-fixture
// test runs.
func ScrubGitHookEnv() {
	for _, v := range gitHookEnvVars {
		_ = os.Unsetenv(v)
	}
}
