//go:build devine2e

package devine2e

import (
	"bufio"
	"context"
	"fmt"
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
			cleanupVerified := false
			t.Cleanup(func() {
				if !cleanupVerified {
					require.NoError(t, cleanupJourneyProcessesAt(registry, ""), "readiness failure still requires owned-child cleanup")
				}
			})
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
print('ready',flush=True)
while True:time.sleep(.01)
`
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, "python3", "-c", script, module, proof, map[bool]string{false: "false", true: "true"}[ignore])
			gracefulJourneyCommand(cmd)
			cmd.WaitDelay = 200 * time.Millisecond
			if !ignore {
				cmd.WaitDelay = 3 * time.Second
			} // the handler itself may wait 2s
			stdout, err := cmd.StdoutPipe()
			require.NoError(t, err)
			require.NoError(t, cmd.Start())
			defer func() {
				cancel()
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
				_ = stdout.Close()
			}()
			ready := make(chan error, 1)
			go func() {
				line, err := bufio.NewReader(stdout).ReadString('\n')
				if err == nil && line != "ready\n" {
					err = fmt.Errorf("unexpected readiness marker")
				}
				ready <- err
			}()
			select {
			case err := <-ready:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("cleanup handler did not become ready")
			}
			// Exercise the same deadline after registration/handler setup. A slow
			// interpreter startup is not evidence that TERM cleanup is broken.
			deadline := time.AfterFunc(300*time.Millisecond, cancel)
			defer deadline.Stop()
			require.Error(t, cmd.Wait(), "the case deadline must still be reported as a failure")
			require.NoError(t, cleanupJourneyProcesses(), "a killed supervisor must not leave its separately grouped CLI running")
			cleanupVerified = true
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
