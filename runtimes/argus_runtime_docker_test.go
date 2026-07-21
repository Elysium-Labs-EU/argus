// Package runtimes_test exercises argus-runtime-docker's --platform
// selection — the piece behind issue #55: a docker run with no --platform
// silently resolves to the daemon default, which fails with a misleading
// "pull access denied" error against an image built with an explicit
// platform pin. It shells out to the real script rather than reimplementing
// its logic in Go.
package runtimes_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runDockerAdapter(t *testing.T, env map[string]string) string {
	t.Helper()

	scriptPath, err := filepath.Abs("argus-runtime-docker")
	if err != nil {
		t.Fatalf("resolving argus-runtime-docker path: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.Env = append(os.Environ(),
		"ARGUS_RUNTIME_WORKTREE="+t.TempDir(),
		"ARGUS_RUNTIME_CMD=claude",
	)
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("argus-runtime-docker failed: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestDockerAdapterDefaultsPlatformToHostArch(t *testing.T) {
	line := runDockerAdapter(t, nil)

	want := "--platform " + "linux/" + runtime.GOARCH
	if !strings.Contains(line, want) {
		t.Errorf("docker run line missing %q: %s", want, line)
	}
}

func TestDockerAdapterHonorsPlatformOverride(t *testing.T) {
	line := runDockerAdapter(t, map[string]string{"ARGUS_RUNTIME_DOCKER_PLATFORM": "linux/riscv64"})

	if !strings.Contains(line, "--platform linux/riscv64") {
		t.Errorf("docker run line ignored ARGUS_RUNTIME_DOCKER_PLATFORM override: %s", line)
	}
}

func TestDockerAdapterPlatformPrecedesImage(t *testing.T) {
	// --platform must land on the docker run invocation itself, before the
	// image name — a flag placed after the image is passed to the
	// container's entrypoint instead of to `docker run`.
	line := runDockerAdapter(t, nil)

	platformIdx := strings.Index(line, "--platform")
	imageIdx := strings.Index(line, "argus-worker:latest")
	if platformIdx == -1 || imageIdx == -1 || platformIdx > imageIdx {
		t.Errorf("--platform must precede the image name: %s", line)
	}
}
