package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
)

const devinCatalogExtraKey = "devin_model_catalog"

func devinInboundModel(ctx context.Context, fallback string) (string, string) {
	if model, ok := RequestedPublicModelFromContext(ctx); ok {
		return model, "composite_ingress"
	}
	if model, ok := ctx.Value(ctxkey.InboundModel).(string); ok && model != "" {
		return model, "http_ingress"
	}
	return fallback, "gateway_entry_fallback"
}

func devinAccountCatalog(a *Account) *devin.CatalogSnapshot {
	if a == nil || a.Extra == nil {
		return nil
	}
	raw := a.Extra[devinCatalogExtraKey]
	if raw == nil {
		return nil
	}
	if snapshot, ok := raw.(*devin.CatalogSnapshot); ok {
		return snapshot
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var snapshot devin.CatalogSnapshot
	if json.Unmarshal(b, &snapshot) != nil {
		return nil
	}
	return &snapshot
}

func devinCatalogExtra(models []*pb.ClientModelConfig, identityRevision string, accounts ...*Account) map[string]any {
	ids := make([]string, 0, len(models))
	for _, m := range models {
		ids = append(ids, devin.ModelID(m))
	}
	snapshot := devin.NewCatalogSnapshot(models, identityRevision)
	var account *Account
	if len(accounts) > 0 {
		account = accounts[0]
	}
	snapshot.Routes = devinEffectiveModelRoutes(models, account)
	return map[string]any{"devin_models": ids, devinCatalogExtraKey: snapshot}
}

// ExplainDevinRouteDenial is used only after scheduling finds no account. It
// distinguishes semantic denial from capacity/auth failure for aliases to
// opaque harness entries. Any account with a supported route preserves the
// ordinary scheduler error; a different account's denial cannot mask it.
func (s *OpenAIGatewayService) ExplainDevinRouteDenial(ctx context.Context, groupID *int64, requested string) error {
	if devinFusionBoundAccount(ctx) > 0 {
		return fusion.E("account_bound", "bound Fusion account or profile is currently unavailable; the session cannot migrate", 409)
	}
	if s == nil || s.accountRepo == nil {
		return nil
	}
	var accounts []Account
	var err error
	if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupID(ctx, *groupID)
	} else {
		accounts, err = s.accountRepo.ListSchedulable(ctx)
	}
	if err != nil {
		return nil
	}
	var denial error
	for i := range accounts {
		a := &accounts[i]
		if a.Platform != PlatformDevin {
			continue
		}
		if mapping := a.GetModelMapping(); len(mapping) > 0 && !mappingSupportsRequestedModel(mapping, requested) {
			continue
		}
		target := a.GetMappedModel(requested)
		if profile := devinFusionProfile(a, target); profile != nil && validateDevinFusionMembers(*profile, devinFusionCatalog(a)) == nil {
			return nil
		}
		if !slices.Contains(a.devinModelIDs(), target) {
			continue
		}
		var models []*pb.ClientModelConfig
		if catalog := devinAccountCatalog(a); catalog != nil {
			models = catalog.Models
		}
		if routeErr := devin.CheckModelIntent(models, requested, target); routeErr != nil {
			denial = routeErr
		} else {
			return nil
		}
	}
	return denial
}

// WriteDevinRouteError returns terminal protocol-compatible errors without
// passing them to account penalties, token recovery or account failover.
func WriteDevinRouteError(c *gin.Context, protocol string, err error) bool {
	var routeErr *devin.RouteError
	if !errors.As(err, &routeErr) {
		return false
	}
	status := routeErr.HTTPStatus()
	typ := "invalid_request_error"
	if status >= 500 {
		typ = "api_error"
	}
	if status < 500 {
		MarkOpsClientBusinessLimited(c, OpsClientBusinessLimitedReasonLocalModelConfiguration)
	}
	c.Set(OpsLocalErrorTypeKey, typ)
	c.Set(OpsLocalErrorCodeKey, routeErr.Code)
	c.Set(ResponseCommittedKey, true)
	if c.Writer.Written() {
		// A concurrency keepalive may already have committed SSE headers before
		// catalog-based scheduling fails. End that stream with its protocol's
		// error event instead of appending a JSON response or retrying an account.
		MarkOpsStreamErrorValue(c, OpsStreamError{ErrType: typ, Code: routeErr.Code, Message: routeErr.Error(), IntendedStatus: status, CountTowardsSLA: status >= 500, RequestScoped: status < 500})
		var event any = gin.H{"error": gin.H{"type": typ, "code": routeErr.Code, "param": "model", "message": routeErr.Message()}}
		if protocol == "anthropic" {
			event = gin.H{"type": "error", "error": gin.H{"type": typ, "message": routeErr.Error()}}
		}
		if protocol == "responses" {
			event = gin.H{"type": "error", "code": routeErr.Code, "param": "model", "message": routeErr.Message()}
		}
		payload, _ := json.Marshal(event)
		if protocol == "chat" {
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", payload)
		} else {
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", payload)
		}
		c.Writer.Flush()
		return true
	}
	if protocol == "anthropic" {
		c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": typ, "message": routeErr.Error()}})
	} else {
		c.JSON(status, gin.H{"error": gin.H{"type": typ, "code": routeErr.Code, "param": "model", "message": routeErr.Message()}})
	}
	return true
}
