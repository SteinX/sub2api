//go:build unit

package service

import (
	"context"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	"github.com/stretchr/testify/require"
)

func contextAccountAccessError(s *OpenAIGatewayService, key *APIKey, a *Account, model string, manual bool) error {
	_, err := s.checkContextAccountAccess(key, a, model, manual)
	return err
}

func TestDevinContextAccountOverridesAndRolloutPrecedence(t *testing.T) {
	cfg, key := contextEnabledFixture()
	a := &Account{ID: 7, Platform: PlatformDevin, Extra: map[string]any{}}
	s := &OpenAIGatewayService{cfg: cfg}
	require.NoError(t, contextAccountAccessError(s, key, a, "gpt-5.5", true), "legacy rollout is preserved")
	a.Extra[DevinContextEnabledExtra] = false
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", false), "account disable overrides global enable")
	cfg.Gateway.DevinContext = config.DevinContextConfig{}
	a.Extra = nil
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", false), "default is closed")
	a.Extra = map[string]any{DevinContextEnabledExtra: true}
	settings, err := s.checkContextAccountAccess(key, a, "gpt-5.5", true)
	require.NoError(t, err, "GUI opt-in needs no YAML edits")
	require.Equal(t, devinContextSettings{Enabled: true, Manual: true, Auto: true}, settings)
	a.Extra[DevinContextManualExtra] = false
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", true))
	require.NoError(t, contextAccountAccessError(s, key, a, "gpt-5.5", false), "manual and inference are independent")
	other := &Account{ID: 8, Platform: PlatformDevin}
	require.Error(t, contextAccountAccessError(s, key, other, "gpt-5.5", false), "one account opt-in does not enable another")
	for _, mutate := range []func(*config.DevinContextConfig){
		func(c *config.DevinContextConfig) { c.ForceDisabled = true },
		func(c *config.DevinContextConfig) { c.APIKeyIDs = []int64{999} },
		func(c *config.DevinContextConfig) { c.ModelUIDs = []string{"other-model"} },
		func(c *config.DevinContextConfig) { c.AccountIDs = []int64{8} },
	} {
		cfg.Gateway.DevinContext = config.DevinContextConfig{}
		mutate(&cfg.Gateway.DevinContext)
		require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", false), "GUI opt-in respects configured restrictions")
	}
	cfg.Gateway.DevinContext = config.DevinContextConfig{Enabled: true}
	a.Extra[DevinContextEnabledExtra] = nil
	require.Error(t, contextAccountAccessError(s, key, a, "gpt-5.5", false), "inherit retains mandatory legacy allowlists")
}

func TestDevinContextCurrentAccountSwitchesGateEveryPaidPhase(t *testing.T) {
	cfg, key := contextEnabledFixture()
	a := &Account{ID: 7, Platform: PlatformDevin, Status: StatusActive, Schedulable: true, GroupIDs: []int64{9}, Extra: devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: "gpt-5.5"}}, "fixture")}
	a.Extra[DevinContextEnabledExtra] = true
	a.Extra[DevinContextAutoExtra] = false
	s := &OpenAIGatewayService{cfg: cfg, accountRepo: &fusionAccountRepo{account: a}}
	x := &contextExecutor{physical: &devinFusionExecutor{service: s, publicModel: "gpt-5.5"}, keys: &APIKeyService{apiKeyRepo: &apiKeyRepoStub{apiKey: key}}, owner: mc.Owner{UserID: 33, KeyID: 22, AccountID: 7, GroupID: 9}}
	ctx := context.Background()
	enabled, err := x.AutoCompactionEnabled(ctx)
	require.NoError(t, err)
	require.False(t, enabled)
	require.NoError(t, x.check(ctx, "inference"))
	_, err = x.Estimate(ctx, mc.Call{Purpose: "compaction"})
	require.Error(t, err, "auto disabled before preparation")
	o, err := x.Execute(ctx, mc.Call{Purpose: "compaction_2"})
	require.Error(t, err, "auto disabled before dispatch")
	require.True(t, o.NotStarted)
	x.manual = true
	require.NoError(t, x.check(ctx, "compaction"), "manual compact remains available with auto off")
	a.Extra[DevinContextManualExtra] = false
	o, err = x.Execute(ctx, mc.Call{Purpose: "compaction"})
	require.Error(t, err, "manual switch is rechecked at dispatch")
	require.True(t, o.NotStarted)
	x.manual = false
	a.Extra[DevinContextAutoExtra] = true
	enabled, err = x.AutoCompactionEnabled(ctx)
	require.NoError(t, err)
	require.True(t, enabled, "turning auto back on affects the same executor/session")
	a.Extra[DevinContextEnabledExtra] = false
	o, err = x.Execute(ctx, mc.Call{Purpose: "inference"})
	require.Error(t, err)
	require.True(t, o.NotStarted)
}

func TestDevinContextNewSessionsRequireEnabledAccount(t *testing.T) {
	cfg, key := contextEnabledFixture()
	cfg.Gateway.DevinContext = config.DevinContextConfig{}
	a := &Account{ID: 7, Platform: PlatformDevin, Status: StatusActive, Schedulable: true, GroupIDs: []int64{9}, Extra: devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: "gpt-5.5"}}, "fixture")}
	s := &OpenAIGatewayService{cfg: cfg, accountRepo: &fusionAccountRepo{account: a}}
	_, err := s.contextAccount(context.Background(), key, 0, "gpt-5.5", false)
	var denied *cc.Error
	require.ErrorAs(t, err, &denied)
	require.Equal(t, "context_disabled", denied.Code)
	a.Extra[DevinContextEnabledExtra] = true
	selected, err := s.contextAccount(context.Background(), key, 0, "gpt-5.5", false)
	require.NoError(t, err)
	require.Equal(t, a.ID, selected.ID)
	a.Extra[DevinContextManualExtra] = false
	_, err = s.contextAccount(context.Background(), key, 0, "gpt-5.5", true)
	require.Error(t, err, "new stateless jobs require the manual switch")
	a.Extra[DevinContextEnabledExtra] = false
	_, err = s.contextAccount(context.Background(), key, 0, "gpt-5.5", false)
	require.Error(t, err, "explicit disable is effective for new sessions too")
}

func TestDevinContextPaidCallsRespectChannelRestrictions(t *testing.T) {
	cfg, key := contextEnabledFixture()
	a := &Account{ID: 7, Platform: PlatformDevin, Status: StatusActive, Schedulable: true, GroupIDs: []int64{9}, Extra: devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: "gpt-5.5"}}, "fixture")}
	s := &OpenAIGatewayService{cfg: cfg, accountRepo: &fusionAccountRepo{account: a}}
	x := &contextExecutor{physical: &devinFusionExecutor{service: s, publicModel: "gpt-5.5"}, keys: &APIKeyService{apiKeyRepo: &apiKeyRepoStub{apiKey: key}}, owner: mc.Owner{UserID: 33, KeyID: 22, AccountID: 7, GroupID: 9}}
	ctx := context.Background()
	for _, allowed := range []bool{false, true} {
		model := "other-model"
		if allowed {
			model = "gpt-5.5"
		}
		channel := Channel{ID: 1, Status: StatusActive, GroupIDs: []int64{9}, RestrictModels: true, ModelPricing: []ChannelModelPricing{{Platform: PlatformDevin, Models: []string{model}}}}
		s.channelService = newTestChannelService(makeStandardRepo(channel, map[int64]string{9: PlatformDevin}))
		if allowed {
			require.NoError(t, s.checkContextChannelAccess(ctx, key, "gpt-5.5"))
			require.NoError(t, x.check(ctx, "inference"))
		} else {
			require.Error(t, s.checkContextChannelAccess(ctx, key, "gpt-5.5"))
			for _, manual := range []bool{false, true} {
				x.manual = manual
				o, err := x.Execute(ctx, mc.Call{Purpose: "compaction"})
				require.Error(t, err)
				require.True(t, o.NotStarted)
			}
		}
	}
}

func TestDevinContextSettingsValidationAndAccountSavePaths(t *testing.T) {
	for _, field := range devinContextExtraKeys {
		for _, valid := range []any{true, false, nil} {
			require.NoError(t, ValidateDevinAccountExtra(PlatformDevin, map[string]any{field: valid}))
		}
		for _, invalid := range []any{"true", 1, []any{true}, map[string]any{"enabled": true}} {
			require.Error(t, ValidateDevinAccountExtra(PlatformDevin, map[string]any{field: invalid}))
		}
		require.Error(t, ValidateDevinAccountExtra(PlatformOpenAI, map[string]any{field: true}))
	}
	ctx := context.Background()
	repo := &upstreamBillingProbeAccountRepo{accounts: map[int64]*Account{
		7: {ID: 7, Platform: PlatformDevin, Extra: map[string]any{"runtime_state": "retain"}},
		8: {ID: 8, Platform: PlatformOpenAI},
	}}
	s := &adminServiceImpl{accountRepo: repo}
	saved, err := s.UpdateAccount(ctx, 7, &UpdateAccountInput{Extra: map[string]any{
		DevinContextEnabledExtra: true, DevinContextManualExtra: false, "runtime_state": "retain",
	}})
	require.NoError(t, err, "the GUI uses the ordinary account update endpoint")
	require.Equal(t, true, saved.Extra[DevinContextEnabledExtra])
	require.Equal(t, false, saved.Extra[DevinContextManualExtra])
	_, err = s.UpdateAccount(ctx, 7, &UpdateAccountInput{Extra: map[string]any{DevinContextEnabledExtra: "true"}})
	require.Error(t, err)
	require.NoError(t, s.UpdateAccountExtra(ctx, 7, map[string]any{DevinContextEnabledExtra: true, DevinContextAutoExtra: false}))
	require.Equal(t, true, repo.accounts[7].Extra[DevinContextEnabledExtra])
	require.Equal(t, "retain", repo.accounts[7].Extra["runtime_state"])
	require.NoError(t, s.UpdateAccountExtra(ctx, 7, map[string]any{DevinContextEnabledExtra: nil}))
	require.Nil(t, repo.accounts[7].Extra[DevinContextEnabledExtra], "merge endpoint can restore inheritance")
	require.Error(t, s.UpdateAccountExtra(ctx, 7, map[string]any{DevinContextAutoExtra: "false"}))
	require.Error(t, s.UpdateAccountExtra(ctx, 8, map[string]any{DevinContextEnabledExtra: true}))
	_, err = s.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{7}, Extra: map[string]any{DevinContextManualExtra: "true"}})
	require.Error(t, err, "bulk updates cannot bypass boolean validation")
	_, err = s.BulkUpdateAccounts(ctx, &BulkUpdateAccountsInput{AccountIDs: []int64{7, 8}, Extra: map[string]any{DevinContextEnabledExtra: true}})
	require.Error(t, err, "mixed-platform bulk update cannot store Devin switches on other accounts")
}
