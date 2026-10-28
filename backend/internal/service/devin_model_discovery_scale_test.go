//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/stretchr/testify/require"
)

func discoveryScaleAccount(t testing.TB, count int) Account {
	t.Helper()
	models := make([]*pb.ClientModelConfig, count)
	description := strings.Repeat("Synthetic catalog capability description. ", 24)
	for i := range models {
		uid := fmt.Sprintf("synthetic-model-%03d", i)
		models[i] = &pb.ClientModelConfig{ModelUid: uid, Label: uid, Description: &description, MaxTokens: 262144,
			ModelInfo: &pb.ModelInfo{ModelUid: uid, MaxOutputTokens: 4096, ModelFeatures: &pb.ModelFeatures{SupportsToolCalls: true}}}
	}
	return Account{ID: 7, Platform: PlatformDevin, Extra: devinCatalogExtra(models, "synthetic-scale")}
}

type countedDiscoveryCatalog struct {
	data  json.RawMessage
	calls atomic.Int64
}

func (c *countedDiscoveryCatalog) MarshalJSON() ([]byte, error) {
	c.calls.Add(1)
	return c.data, nil
}

func TestDevinDiscoveryDecodesOneCatalogPerAccount(t *testing.T) {
	account := discoveryScaleAccount(t, 239)
	raw, err := json.Marshal(account.Extra[devinCatalogExtraKey])
	require.NoError(t, err)
	encoded := &countedDiscoveryCatalog{data: raw}
	account.Extra[devinCatalogExtraKey] = encoded
	group := int64(9)
	repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{group: {account}}}
	svc := &GatewayService{accountRepo: repo}
	started := time.Now()
	models := svc.GetAvailableModels(context.Background(), &group, PlatformDevin)
	t.Logf("synthetic catalog: models=%d bytes=%d elapsed=%s", len(models), len(raw), time.Since(started))
	require.Len(t, models, 239)
	require.EqualValues(t, 1, encoded.calls.Load(), "bulk discovery must not decode the complete persisted catalog for each candidate UID")
	require.Same(t, encoded, account.Extra[devinCatalogExtraKey], "request preparation must not mutate the repository's shared account")
	// A subsequent request must see a changed grant, rather than reuse a
	// process-wide decoded catalog or the independent model-list TTL.
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(raw, &snapshot))
	snapshot["models"].([]any)[0].(map[string]any)["disabled"] = true
	encoded.data, err = json.Marshal(snapshot)
	require.NoError(t, err)
	require.Len(t, svc.GetAvailableModels(context.Background(), &group, PlatformDevin), 238)
	require.EqualValues(t, 2, encoded.calls.Load())
	require.Same(t, encoded, account.Extra[devinCatalogExtraKey])
}

func BenchmarkDevinDiscoverySerializedCatalog(b *testing.B) {
	for _, representation := range []string{"typed", "json", "mixed-typed", "mixed-json"} {
		b.Run(representation, func(b *testing.B) {
			account := discoveryScaleAccount(b, 239)
			if strings.HasPrefix(representation, "mixed-") {
				catalog := devinAccountCatalog(&account)
				for i := 120; i < 239; i++ {
					uid := "fusion-" + catalog.Models[i-119].ModelUid + "-sidekick-" + catalog.Models[0].ModelUid
					catalog.Models[i].ModelUid = uid
					catalog.Models[i].ModelInfo.ModelUid = uid
					catalog.Models[i].ModelInfo.HarnessUids = []string{"opaque-native-fusion"}
				}
				account.Extra = devinCatalogExtra(catalog.Models, "synthetic-mixed")
			}
			if strings.HasSuffix(representation, "json") {
				raw, err := json.Marshal(account.Extra)
				require.NoError(b, err)
				require.NoError(b, json.Unmarshal(raw, &account.Extra))
			}
			group := int64(9)
			repo := &modelsListAccountRepoStub{byGroup: map[int64][]Account{group: {account}}}
			svc := &GatewayService{accountRepo: repo}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if len(svc.GetAvailableModels(context.Background(), &group, PlatformDevin)) != 239 {
					b.Fatal("catalog changed")
				}
			}
		})
	}
}
