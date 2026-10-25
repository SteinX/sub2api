//go:build devine2e

package devine2e

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestApprovedJourneyDeadlineAllowsCleanupAndReapsTrackedGroups(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		t.Run(map[bool]string{false: "graceful", true: "forced-fallback"}[ignore], func(t *testing.T) {
			directory := t.TempDir()
			registry := filepath.Join(directory, "registry")
			proof := filepath.Join(directory, "cleaned")
			require.NoError(t, os.WriteFile(registry, nil, 0600))
			t.Setenv("DEVIN_JOURNEY_PROCESS_REGISTRY", registry)
			module, err := filepath.Abs("../../../tools/devin-e2e-clients")
			require.NoError(t, err)
			script := `import os,signal,subprocess,sys,time
sys.path.insert(0,sys.argv[1])
import process_cleanup
p=subprocess.Popen([sys.executable,'-c','import time;time.sleep(30)'],start_new_session=True)
process_cleanup.register(p)
def stop(*args):
 os.killpg(p.pid,signal.SIGTERM);p.wait(timeout=2)
 open(sys.argv[2],'w').write('cleaned');sys.exit(0)
signal.signal(signal.SIGTERM,signal.SIG_IGN if sys.argv[3]=='true' else stop)
while True:time.sleep(.01)
`
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			cmd := exec.CommandContext(ctx, "python3", "-c", script, module, proof, map[bool]string{false: "false", true: "true"}[ignore])
			gracefulJourneyCommand(cmd)
			cmd.WaitDelay = 200 * time.Millisecond
			require.Error(t, cmd.Run(), "the case deadline must still be reported as a failure")
			require.NoError(t, cleanupJourneyProcesses(), "a killed supervisor must not leave its separately grouped CLI running")
			if !ignore {
				_, err := os.Stat(proof)
				require.NoError(t, err, "TERM must reach the cleanup handler before hard kill")
			}
			data, err := os.ReadFile(registry)
			require.NoError(t, err)
			require.Contains(t, string(data), "start_ticks")
			// The process checker deliberately reports no PIDs or subprocess output.
			check := exec.Command("python3", "-c", `import json,pathlib,sys
for row in map(json.loads,pathlib.Path(sys.argv[1]).read_text().splitlines()):
 try: state=pathlib.Path('/proc/%s/stat'%row['pid']).read_text().rsplit(')',1)[1].split()[0]
 except FileNotFoundError: continue
 assert state=='Z', 'tracked child remains running'
`, registry)
			require.NoError(t, check.Run())
		})
	}
}
