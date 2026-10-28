//go:build unit

package service

import (
	"context"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	gocache "github.com/patrickmn/go-cache"
	"github.com/stretchr/testify/require"
)

// The Use Key modal reads this public list after account/catalog edits. A
// second independent TTL must not hide new grants or resurrect removed ones.
func TestDevinPublicDiscoveryReflectsCatalogAndPolicyUpdates(t *testing.T) {
	groupID := int64(9)
	account := Account{ID: 7, Platform: PlatformDevin, Extra: map[string]any{}}
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{groupID: {account}}}
	svc := &GatewayService{accountRepo: repo, modelsListCache: gocache.New(time.Hour, time.Hour), modelsListCacheTTL: time.Hour}
	list := func() []string { return svc.GetAvailableModels(context.Background(), &groupID, PlatformDevin) }
	require.Empty(t, list(), "establish the cached missing-catalog precondition")
	profile := fusionTestProfile()
	account.Extra = devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: profile.LeadModel}, {ModelUid: profile.SidekickModel}}, "revision-1")
	account.Extra[fusion.ExtraProfiles] = []fusion.Profile{profile}
	repo.byGroup[groupID] = []Account{account}
	require.Contains(t, list(), profile.PublicModel, "the next discovery must expose newly configured Fusion routes")
	profile.Enabled = false
	account.Extra[fusion.ExtraProfiles] = []fusion.Profile{profile}
	require.NotContains(t, list(), profile.PublicModel, "disabled profiles must disappear immediately")
	profile.Enabled = true
	account.Extra = devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: profile.LeadModel}}, "revision-2")
	account.Extra[fusion.ExtraProfiles] = []fusion.Profile{profile}
	repo.byGroup[groupID] = []Account{account}
	require.NotContains(t, list(), profile.PublicModel, "a removed physical member invalidates the pair")
	require.NotContains(t, list(), profile.SidekickModel)
	account.Extra = devinCatalogExtra([]*pb.ClientModelConfig{{ModelUid: profile.LeadModel}, {ModelUid: profile.SidekickModel}}, "revision-3")
	account.Extra[fusion.ExtraProfiles] = []fusion.Profile{profile}
	repo.byGroup[groupID] = []Account{account}
	require.Contains(t, list(), profile.PublicModel, "restoring current grants restores the route")
}
