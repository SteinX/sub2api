//go:build devine2e

package devine2e

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Actual CLI observations use the real Stop command and file-locked ledger.
// Admission/settlement below call the same ledger core in separate processes;
// they cannot start the real runner: this fixture's approval is explicitly false.
type fixtureLedgerBudget struct {
	accounting                      *authorizedJourneyBudget
	plan, scope, ledger, policyRoot string
	settled                         chan error
}

func fixturePolicyEnvironment() []string {
	var result []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if !strings.Contains(" DEVIN_E2E_PAT DEVIN_E2E_CREDENTIAL DEVIN_PAT DEVIN_API_KEY SUB2API_API_KEY SUB2API_USER_TOKEN DEVIN_JOURNEY_PLAN_FILE DEVIN_JOURNEY_APPROVAL_FILE DEVIN_JOURNEY_RUN_LEDGER ", " "+name+" ") {
			result = append(result, value)
		}
	}
	return result
}

func (b *fixtureLedgerBudget) command(arguments ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "python3", arguments...)
	command.Env = fixturePolicyEnvironment()
	return command.Output()
}

const fixtureLedgerAction = `import json,sys
sys.path.insert(0,sys.argv[1])
from journey_policy import ledger_action,read_json,digest,Denied
plan=read_json(sys.argv[3]);scope={'plan_sha256':digest(plan),'mode':'live','source':plan['source']}
action=sys.argv[4];kwargs={'expected_scope':scope}
if action=='admit':kwargs['cost']=float(sys.argv[5])
if action=='complete':kwargs.update(receipt=int(sys.argv[5]),outcome=sys.argv[6],cost=float(sys.argv[7]))
if action=='tool_begin':kwargs['operation']=sys.argv[5]
if action=='tool_reserve':kwargs.update(operation=sys.argv[5],calls=json.loads(sys.argv[6]))
try:
 state=ledger_action(sys.argv[2],action,**kwargs)
 print(json.dumps({'authorized':not state['stopped'],'receipt':state['inflight'],'stopped':state['stopped']}))
except Denied as error:
 print(json.dumps({'authorized':False,'reason':str(error)}))
`

func newFixtureLedgerBudget(t *testing.T, accounting *authorizedJourneyBudget, longScope ...bool) *fixtureLedgerBudget {
	t.Helper()
	policyRoot, err := filepath.Abs("../../../tools/devin-acceptance")
	require.NoError(t, err)
	var random [12]byte
	_, err = rand.Read(random[:])
	require.NoError(t, err)
	identity := "fixture_cli_stop_" + hex.EncodeToString(random[:])
	ledger := filepath.Join(policyRoot, "../../.cache/devin-approved-runs", identity+".json")
	ledger, err = filepath.Abs(ledger)
	require.NoError(t, err)
	directory := t.TempDir()
	b := &fixtureLedgerBudget{accounting: accounting, plan: filepath.Join(directory, "plan.json"), scope: filepath.Join(directory, "nonexecuting-scope.json"), ledger: ledger, policyRoot: policyRoot, settled: make(chan error, 64)}
	initialize := `import json,sys
sys.path.insert(0,sys.argv[1])
from journey_policy import claim,digest,TOOL_OPERATIONS,LONG_OPERATIONS
plan={'source':{'commit':'a'*40,'tree':'b'*40},'clients':['opencode'],
      'tool_budget':{'operations':TOOL_OPERATIONS,'per_operation':12,'total':48},
      'limits':{'public_requests':40,'local_usd':1,'turn_usd':.5,'wall_seconds':300,'tools_per_turn':12}}
if sys.argv[6]=='long':
 plan.update(journey_set='core_with_long_v1',dataset='synthetic_price_project_long_v1')
 plan['tool_budget'].update(operations=TOOL_OPERATIONS+LONG_OPERATIONS,total=60,no_tools_operations=[LONG_OPERATIONS[1]])
scope={'schema_version':1,'approved':False,'approval_id':sys.argv[5],'plan_sha256':digest(plan)}
for path,value in [(sys.argv[2],plan),(sys.argv[3],scope)]:
 with open(path,'w') as file:json.dump(value,file)
claim(sys.argv[4],plan,'live')
`
	kind := "short"
	if len(longScope) > 0 && longScope[0] {
		kind = "long"
	}
	_, err = b.command("-c", initialize, policyRoot, b.plan, b.scope, b.ledger, identity, kind)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.Remove(b.ledger)) })
	for _, path := range []string{b.plan, b.scope} {
		require.NoError(t, os.Chmod(path, 0600))
	}
	// The scope authorizes stopping this fixture, never native auth/generation.
	output, denied := b.command(filepath.Join(policyRoot, "authorized_journey.py"), "admit", "--mode", "live", "--plan", b.plan, "--approval", b.scope, "--ledger", b.ledger)
	require.Error(t, denied)
	var decision map[string]any
	require.NoError(t, json.Unmarshal(output, &decision))
	require.Equal(t, "explicit_approval_missing", decision["reason"])
	return b
}

func (b *fixtureLedgerBudget) Admit() (int, error) {
	cost, err := b.accounting.cost()
	if err != nil {
		return 0, err
	}
	output, err := b.command("-c", fixtureLedgerAction, b.policyRoot, b.ledger, b.plan, "admit", fmt.Sprint(cost))
	var decision struct {
		Authorized bool `json:"authorized"`
		Receipt    int  `json:"receipt"`
	}
	if err != nil || json.Unmarshal(output, &decision) != nil || !decision.Authorized || decision.Receipt < 1 {
		return 0, errors.New("synthetic ledger denied")
	}
	return decision.Receipt, nil
}

func (b *fixtureLedgerBudget) Complete(receipt int, outcome string) error {
	cost, err := b.accounting.cost()
	if err == nil {
		output, callErr := b.command("-c", fixtureLedgerAction, b.policyRoot, b.ledger, b.plan, "complete", fmt.Sprint(receipt), outcome, fmt.Sprint(cost))
		var decision struct {
			Authorized bool `json:"authorized"`
		}
		if callErr != nil || json.Unmarshal(output, &decision) != nil || !decision.Authorized {
			err = errors.New("synthetic settlement unavailable")
		}
	}
	b.settled <- err
	return err
}

func (b *fixtureLedgerBudget) Stop(reason string, observed int64) error {
	_, err := b.command(filepath.Join(b.policyRoot, "authorized_journey.py"), "stop", "--mode", "live", "--plan", b.plan, "--approval", b.scope, "--ledger", b.ledger, "--reason", reason, "--observed-ns", fmt.Sprint(observed))
	return err
}

func (b *fixtureLedgerBudget) toolPolicy(action, operation string, calls []string) error {
	arguments := []string{"-c", fixtureLedgerAction, b.policyRoot, b.ledger, b.plan, action, operation}
	if calls != nil {
		encoded, err := json.Marshal(calls)
		if err != nil {
			return err
		}
		arguments = append(arguments, string(encoded))
	}
	output, err := b.command(arguments...)
	var decision struct {
		Authorized bool `json:"authorized"`
		Stopped    bool `json:"stopped"`
	}
	if err != nil || json.Unmarshal(output, &decision) != nil || !decision.Authorized || decision.Stopped {
		return errors.New("synthetic tool ledger denied or unavailable")
	}
	return nil
}

func (b *fixtureLedgerBudget) ToolBegin(operation string) error {
	return b.toolPolicy("tool_begin", operation, nil)
}
func (b *fixtureLedgerBudget) ToolReserve(operation string, calls []string) error {
	return b.toolPolicy("tool_reserve", operation, calls)
}

func (b *fixtureLedgerBudget) verifyStopped(t *testing.T, expectedRequests int) map[string]any {
	t.Helper()
	select {
	case err := <-b.settled:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("initial request settlement did not finish")
	}
	before, err := os.ReadFile(b.ledger)
	require.NoError(t, err)
	var state map[string]any
	require.NoError(t, json.Unmarshal(before, &state))
	require.Equal(t, true, state["stopped"])
	require.Equal(t, false, state["closed"])
	require.Nil(t, state["inflight"])
	require.EqualValues(t, expectedRequests, state["requests"])
	require.Greater(t, state["deadline_unix"].(float64), float64(time.Now().UnixNano())/1e9)
	require.Less(t, state["local_cost_usd"].(float64), float64(1))
	// A new interpreter attempts admission with unchanged source/plan/ledger.
	output, err := b.command("-c", fixtureLedgerAction, b.policyRoot, b.ledger, b.plan, "admit", fmt.Sprint(state["local_cost_usd"]))
	require.NoError(t, err)
	var decision map[string]any
	require.NoError(t, json.Unmarshal(output, &decision))
	require.Equal(t, false, decision["authorized"])
	require.Equal(t, "run_stopped_or_previous_send_uncertain", decision["reason"])
	after, err := os.ReadFile(b.ledger)
	require.NoError(t, err)
	require.Equal(t, before, after, "denied restart cannot mutate/reopen the existing ledger")
	return map[string]any{"verified": true, "synthetic_scope_only": true, "scope_cannot_authorize_generation": true, "stop_command": "authorized_journey.py stop", "restart_admission": "journey_policy.ledger_action in an independent interpreter", "restart_denial_reason": decision["reason"], "other_admission_conditions_valid": true, "requests_before_restart": state["requests"], "requests_after_restart": state["requests"], "ledger_unchanged_after_denial": true, "ledger_stopped": true, "settled_inflight": true}
}
