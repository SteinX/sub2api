package handler

import (
	"errors"

	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// Reject named special intents before scheduling. Otherwise removing them from
// the effective catalog would turn a precise policy error into an account 503.
func rejectUnsupportedDevinIntent(c *gin.Context, key *service.APIKey, model, protocol string, gateways ...*service.OpenAIGatewayService) bool {
	if openAICompatibleRequestPlatform(c.Request.Context(), key) != service.PlatformDevin {
		return false
	}
	if service.DevinFusionRequest(c.Request.Context()) {
		return false
	}
	if len(gateways) > 0 && gateways[0] != nil && gateways[0].HasDevinFusionProfile(c.Request.Context(), key.GroupID, model) {
		return false
	}
	err := devin.CheckModelIntent(nil, clientRequestedModel(c, model), model)
	if err == nil {
		return false
	}
	var routeErr *devin.RouteError
	if !errors.As(err, &routeErr) {
		return false
	}
	traceID, _ := c.Request.Context().Value(ctxkey.RequestID).(string)
	if traceID == "" {
		traceID = uuid.NewString()
	}
	traceID = devin.DiagnosticModelID(traceID)
	c.Header("X-Sub2API-Request-Id", traceID)
	logger.FromContext(c.Request.Context()).Info("devin.route_decision", zap.String("component", "audit.devin.routing"), zap.String("platform", service.PlatformDevin), zap.String("request_id", traceID), zap.String("route_mode", devin.RouteBlocked), zap.String("decision_code", routeErr.Code), zap.String("outcome", "rejected_before_scheduling"))
	return service.WriteDevinRouteError(c, protocol, err)
}

func rejectUnavailableDevinRoute(c *gin.Context, key *service.APIKey, gateway *service.OpenAIGatewayService, model, protocol string) bool {
	if openAICompatibleRequestPlatform(c.Request.Context(), key) != service.PlatformDevin {
		return false
	}
	if err := gateway.ExplainDevinRouteDenial(c.Request.Context(), key.GroupID, model); err != nil {
		return service.WriteDevinFusionError(c, protocol, err) || service.WriteDevinRouteError(c, protocol, err)
	}
	return false
}

func prepareDevinFusionRequest(c *gin.Context, key *service.APIKey, gateway *service.OpenAIGatewayService, updater service.APIKeyQuotaUpdater, model string, body []byte, protocol string) bool {
	if openAICompatibleRequestPlatform(c.Request.Context(), key) != service.PlatformDevin {
		return false
	}
	_, err := gateway.BindDevinFusionRequest(c, key, model, body, protocol, updater)
	if err == nil {
		return false
	}
	if service.DevinFusionHandled(err) {
		return true
	}
	return service.WriteDevinFusionError(c, protocol, err)
}
