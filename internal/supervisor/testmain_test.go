package supervisor

import (
	"os"
	"testing"

	"github.com/Elysium-Labs-EU/argus/internal/testenv"
)

func TestMain(m *testing.M) {
	testenv.ScrubGitHookEnv()
	os.Exit(m.Run())
}
