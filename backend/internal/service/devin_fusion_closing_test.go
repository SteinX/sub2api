//go:build unit

package service

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDevinFusionClosingSelectorIsStrictAndSessionScoped(t *testing.T) {
	f := newWarmFixture(t)
	for _, values := range [][]string{nil, {fusion.ClosingReserveV1}, {""}, {"off"}, {"reserve-v2"}, {" reserve-v1"}, {fusion.ClosingReserveV1, fusion.ClosingReserveV1}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
		for _, v := range values {
			c.Request.Header.Add(fusion.HeaderClosingPolicy, v)
		}
		_, err := f.service.BindDevinFusionRequest(c, f.keys.key, f.session.PublicModel, fusionProtocolBody("responses", false, ""), "responses", f.keys)
		if len(values) == 0 || len(values) == 1 && values[0] == fusion.ClosingReserveV1 {
			require.NoError(t, err)
			b, _ := c.Request.Context().Value(devinFusionBindingKey{}).(*devinFusionBinding)
			require.NotNil(t, b)
			if len(values) > 0 {
				require.Equal(t, fusion.ClosingReserveV1, b.ClosingPolicy)
			}
		} else {
			require.ErrorContains(t, err, "closing_policy_invalid")
		}
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set(fusion.HeaderSession, f.session.ID)
	c.Request.Header.Set(fusion.HeaderClosingPolicy, fusion.ClosingReserveV1)
	_, err := f.service.BindDevinFusionRequest(c, f.keys.key, f.session.PublicModel, fusionProtocolBody("responses", false, ""), "responses", f.keys)
	require.ErrorContains(t, err, "closing_policy_conflict")
	require.Empty(t, devinFusionProfile(f.account, f.session.PublicModel).ClosingPolicy)
	require.Zero(t, f.calls.Load())
}

func TestFusionWarmClosingOptInPreservesMutableAccountChecks(t *testing.T) {
	f := newWarmFixture(t)
	f.session.Profile.ClosingPolicy = fusion.ClosingReserveV1
	f.session.ProfileRevision = f.session.Profile.Revision()
	f.save(t)
	f.snapshot.Profile = f.session.Profile
	f.snapshot.ProfileRevision = f.session.ProfileRevision
	f.snapshot.LaneScope = f.session.LaneScope(fusion.Lead)
	_, err := f.adapter.Estimate(context.Background(), f.snapshot)
	require.NoError(t, err)
	require.Empty(t, devinFusionProfile(f.account, f.session.PublicModel).ClosingPolicy)
	p := *devinFusionProfile(f.account, f.session.PublicModel)
	p.MaxCostUSD += 1
	f.account.Extra[fusion.ExtraProfiles] = []fusion.Profile{p}
	_, err = f.adapter.Estimate(context.Background(), f.snapshot)
	require.ErrorContains(t, err, "authorization or route changed")
	require.Zero(t, f.calls.Load())
}
