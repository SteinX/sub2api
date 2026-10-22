package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

func (s *OpenAIGatewayService) SetDevinService(d *DevinService) { s.devin = d }

func (s *OpenAIGatewayService) forwardDevin(ctx context.Context, c *gin.Context, a *Account, body []byte, protocol, cacheKey, mapped string) (*OpenAIForwardResult, error) {
	ctx = devin.WithClientIdentity(ctx)
	started := time.Now()
	if s.devin == nil {
		return nil, fmt.Errorf("devin service is unavailable")
	}
	var req *apicompat.ResponsesRequest
	var err error
	switch protocol {
	case "anthropic":
		var r apicompat.AnthropicRequest
		if err = json.Unmarshal(body, &r); err == nil {
			req, err = apicompat.AnthropicToResponsesWithMinOutputTokens(&r, 1)
		}
	case "chat":
		var r apicompat.ChatCompletionsRequest
		if err = json.Unmarshal(body, &r); err == nil {
			req, err = apicompat.ChatCompletionsToResponsesWithMinOutputTokens(&r, 1)
		}
	default:
		req = &apicompat.ResponsesRequest{}
		err = json.Unmarshal(body, req)
	}
	writeError := func(status int, message string) {
		if protocol == "anthropic" {
			c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": message}})
		} else {
			c.JSON(status, gin.H{"error": gin.H{"type": "api_error", "message": message}})
		}
	}
	if err != nil {
		writeError(400, "Invalid request: "+err.Error())
		return nil, err
	}
	// Shared converters force streaming for HTTP upstreams; preserve the client's choice here.
	var inbound struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &inbound)
	req.Stream = inbound.Stream
	original := req.Model
	req.Model = resolveOpenAIForwardModel(a, req.Model, mapped)
	if profile := devinFusionProfile(a, req.Model); profile != nil {
		return s.forwardDevinFusion(ctx, c, a, body, protocol, original, *profile)
	}
	if DevinFusionRequest(ctx) {
		err := fusion.E("account_bound", "the bound Fusion account or profile is unavailable", 409)
		WriteDevinFusionError(c, protocol, err)
		return &OpenAIForwardResult{Model: original, FusionUsageSettled: true}, err
	}
	inboundModel, inboundSource := devinInboundModel(ctx, original)
	traceID, _ := ctx.Value(ctxkey.RequestID).(string)
	if traceID == "" {
		traceID = uuid.NewString()
	}
	traceID = devin.DiagnosticModelID(traceID)
	if len(traceID) > 64 {
		traceID = uuid.NewString()
	}
	c.Header("X-Sub2API-Request-Id", traceID)
	trace := &devin.ModelRouteTrace{TraceID: traceID, ClientRequestedModel: devin.DiagnosticModelID(inboundModel), GatewayEntryModel: devin.DiagnosticModelID(original), MappedModel: devin.DiagnosticModelID(req.Model), SchemaRevision: devin.CatalogSchemaRevision, PolicyRevision: devin.RoutePolicyRevision, IdentityRevision: devin.ResolveClientIdentity(ctx).Revision, RouteMode: devin.RouteBlocked, MechanismStatus: "unverified", Outcome: "failed"}
	trace.ClientRequestedModelSource, trace.DecisionPhase = inboundSource, "preflight"
	if inboundModel != original {
		trace.MappingSteps = append(trace.MappingSteps, devin.RouteMapping{Stage: "handler_channel", From: trace.ClientRequestedModel, To: trace.GatewayEntryModel})
	}
	if original != req.Model {
		// Account mappings take precedence; the dispatch hint is only used when
		// no account mapping matched. Do not invent a step through an unused hint.
		stage := "account_alias"
		if _, matched := a.ResolveMappedModel(original); !matched && strings.TrimSpace(mapped) != "" {
			stage = "gateway_dispatch"
		}
		trace.MappingSteps = append(trace.MappingSteps, devin.RouteMapping{Stage: stage, From: trace.GatewayEntryModel, To: trace.MappedModel})
	}
	c.Set("devin_route_trace", trace)
	defer func() {
		log := logger.FromContext(ctx).With(zap.String("component", "audit.devin.routing"), zap.String("platform", PlatformDevin))
		log.Info("devin.route_decision", zap.String("request_id", traceID), zap.String("route_mode", trace.RouteMode), zap.String("decision_code", trace.DecisionCode), zap.String("outcome", trace.Outcome))
		// Detailed identity/usage evidence is opt-in through the existing debug
		// log level and authenticated admin system-log viewer, with bounded data.
		log.Debug("devin.route_trace", zap.String("request_id", traceID), zap.Any("devin_route", trace))
	}()
	rejectRoute := func(err error) error {
		var routeErr *devin.RouteError
		if errors.As(err, &routeErr) {
			trace.DecisionCode = routeErr.Code
			trace.DecisionOrigin = routeErr.Origin()
		}
		WriteDevinRouteError(c, protocol, err)
		return err
	}
	if err = devin.CheckModelIntent(nil, inboundModel, original, req.Model); err != nil {
		for _, uid := range []string{inboundModel, original, req.Model} {
			if p := devin.PlanModelUID(uid); uid != "" && p.Mode == devin.RouteBlocked {
				trace.CatalogKind = p.CatalogKind
				break
			}
		}
		return nil, rejectRoute(err)
	}
	if strings.TrimSpace(cacheKey) == "" {
		cacheKey = req.PromptCacheKey
	}
	if cacheKey == "" {
		cacheKey = extractClaudeCodeSessionID(c, body)
	}
	if cacheKey == "" {
		cacheKey = "request:" + c.GetHeader("X-Request-Id") + fmt.Sprint(time.Now().UnixNano())
	}
	wire, err := devin.BuildRequest(req, fmt.Sprintf("%d:%d:%s", getAPIKeyIDFromContext(c), a.ID, cacheKey))
	if err != nil {
		writeError(400, err.Error())
		return nil, err
	}
	models, err := s.devin.Models(ctx, a)
	if err != nil {
		return nil, s.devinForwardFailure(c, a, err)
	}
	trace.CatalogRevision = devin.CatalogRevision(models)
	selected := devin.FindModel(models, req.Model)
	if selected == nil {
		return nil, rejectRoute(&devin.RouteError{Code: devin.CodeModelUnavailable})
	}
	plan := devin.PlanModel(selected)
	trace.CatalogKind, trace.RouteMode, trace.MechanismStatus = plan.CatalogKind, plan.Mode, plan.MechanismStatus
	trace.CatalogHarnessUIDs = devin.DiagnosticIDs(plan.HarnessUIDs)
	trace.CapabilitySource = "selected_catalog"
	if err = devin.CheckModelIntent(models, inboundModel, original, req.Model); err != nil {
		return nil, rejectRoute(err)
	}
	if err = plan.Validate(); err != nil {
		return nil, rejectRoute(err)
	}
	if err = devin.ValidateModelCapabilities(wire, selected, req.MaxOutputTokens); err != nil {
		return nil, rejectRoute(err)
	}
	client, err := s.devin.Client(ctx, a)
	if err != nil {
		return nil, err
	}
	token, err := s.devin.Token(ctx, a, false)
	if err != nil {
		return nil, s.devinForwardFailure(c, a, err)
	}
	anthState := apicompat.NewResponsesEventToAnthropicState()
	chatState := apicompat.NewResponsesEventToChatState()
	var firstToken *int
	clientDisconnected := false
	seq := 0
	emit := func(e apicompat.ResponsesStreamEvent) error {
		if !req.Stream || clientDisconnected {
			return nil
		}
		if ctx.Err() != nil {
			clientDisconnected = true
			return nil
		}
		if !c.Writer.Written() {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("X-Accel-Buffering", "no")
		}
		write := func(text string) error {
			if ctx.Err() != nil {
				clientDisconnected = true
			}
			if clientDisconnected {
				return nil
			}
			_, err := c.Writer.Write([]byte(text))
			if err != nil {
				clientDisconnected = true
				return nil
			}
			c.Writer.Flush()
			return nil
		}
		e.SequenceNumber = seq
		seq++
		switch protocol {
		case "anthropic":
			for _, evt := range apicompat.ResponsesEventToAnthropicEvents(&e, anthState) {
				text, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					return err
				}
				if err = write(text); err != nil {
					return err
				}
			}
		case "chat":
			for _, evt := range apicompat.ResponsesEventToChatChunks(&e, chatState) {
				text, err := apicompat.ChatChunkToSSE(evt)
				if err != nil {
					return err
				}
				if err = write(text); err != nil {
					return err
				}
			}
		default:
			b, err := json.Marshal(e)
			if err != nil {
				return err
			}
			return write("event: " + e.Type + "\ndata: " + string(b) + "\n\n")
		}
		return nil
	}
	decoder := devin.NewEventDecoder(original, emit)
	drainInterval := 3 * time.Minute
	if s.cfg != nil && s.cfg.Gateway.StreamDataIntervalTimeout > 0 {
		drainInterval = time.Duration(s.cfg.Gateway.StreamDataIntervalTimeout) * time.Second
	}
	for attempt := 0; attempt < 2; attempt++ {
		trace.DecisionPhase = "assignment"
		wire.ChatModelUid = req.Model
		wire.ModelAssignmentJwt = nil
		prepared, prepErr := client.Prepare(ctx, token, wire, plan, models)
		attemptTrace := devin.RouteAttempt{Number: attempt + 1, AssignmentUsed: prepared.AssignmentUsed, AssignedModel: devin.DiagnosticModelID(prepared.AssignedModelUID), AssignedHarnessUIDs: devin.DiagnosticIDs(prepared.AssignedHarnessUIDs)}
		err = prepErr
		if err == nil {
			finalModel := devin.FindModel(models, prepared.FinalModelUID)
			trace.DecisionPhase = "final_capabilities"
			if prepared.AssignmentUsed {
				trace.CapabilitySource = "router_catalog"
				if finalModel != nil {
					trace.CapabilitySource = "assigned_catalog"
				}
			}
			err = devin.ValidateModelCapabilities(wire, finalModel, req.MaxOutputTokens)
		}
		if err == nil {
			attemptTrace.WireModel = devin.DiagnosticModelID(wire.ChatModelUid)
			trace.DecisionPhase = "generation"
			attemptTrace.GenerationAttempted = ctx.Err() == nil
			err = consumeDevinChat(ctx, c, client, prepared.BaseURL, token, wire, decoder, req.Stream, &firstToken, &clientDisconnected, drainInterval, started)
		}
		trace.Attempts = append(trace.Attempts, attemptTrace)
		if err == nil {
			err = decoder.Finish()
		}
		if clientDisconnected || ctx.Err() != nil || !devin.IsUnauthenticated(err) || decoder.Started || attempt > 0 || a.GetCredential("api_key") == "" {
			break
		}
		token, err = s.devin.RecoverToken(ctx, a, token)
		if err != nil {
			break
		}
	}
	billingModel := decoder.BillingModel
	trace.BillingSource = "upstream_usage"
	if billingModel == "" {
		billingModel = req.Model
		trace.BillingSource = "mapped_request_fallback"
	}
	trace.BillingModel, trace.Evidence = devin.DiagnosticModelID(billingModel), decoder.Evidence
	if decoder.Response.Usage == nil {
		trace.BillingModel, trace.BillingSource = "", ""
	}
	result := &OpenAIForwardResult{RequestID: traceID, Model: original, BillingModel: billingModel, UpstreamModel: wire.ChatModelUid, UpstreamResponseModel: decoder.ActualModel, UpstreamResponseModelConflict: len(decoder.Evidence.ActualModels) > 1, ResponseID: decoder.Response.ID, Stream: req.Stream, Duration: time.Since(started), FirstTokenMs: firstToken, ClientDisconnect: clientDisconnected || ctx.Err() != nil, UpstreamEndpoint: devin.ChatPath, DevinRoute: trace}
	if u := decoder.Response.Usage; u != nil {
		result.Usage = OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens}
		if u.InputTokensDetails != nil {
			result.Usage.CacheReadInputTokens = u.InputTokensDetails.CachedTokens
		}
	}
	if result.ClientDisconnect {
		trace.Outcome = "client_disconnected"
		return result, err
	}
	if err != nil {
		var routeErr *devin.RouteError
		if errors.As(err, &routeErr) {
			trace.DecisionCode = routeErr.Code
			trace.DecisionOrigin = routeErr.Origin()
		}
		if !c.Writer.Written() {
			if routeErr != nil {
				WriteDevinRouteError(c, protocol, routeErr)
				if decoder.Response.Usage != nil {
					return result, err
				}
				return nil, err
			}
			if decoder.Started {
				writeError(502, "Devin generation did not complete")
				return result, err
			}
			return nil, s.devinForwardFailure(c, a, err)
		}
		code, message := "upstream_error", "Devin stream failed"
		if routeErr != nil {
			code, message = routeErr.Code, routeErr.Error()
		}
		switch protocol {
		case "anthropic":
			b, _ := json.Marshal(gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": message}})
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", b)
		case "chat":
			b, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "code": code, "message": message}})
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", b)
		default:
			_ = emit(apicompat.ResponsesStreamEvent{Type: "response.failed", Response: &apicompat.ResponsesResponse{ID: decoder.Response.ID, Object: "response", CreatedAt: decoder.Response.CreatedAt, Model: original, Status: "failed", Output: decoder.Response.Output, Error: &apicompat.ResponsesError{Code: code, Message: message}}})
		}
		c.Writer.Flush()
		return result, err
	}
	if !req.Stream {
		switch protocol {
		case "anthropic":
			c.JSON(200, apicompat.ResponsesToAnthropic(&decoder.Response, original))
		case "chat":
			c.JSON(200, apicompat.ResponsesToChatCompletions(&decoder.Response, original))
		default:
			c.JSON(200, decoder.Response)
		}
	} else if protocol == "chat" {
		if _, writeErr := c.Writer.WriteString("data: [DONE]\n\n"); writeErr != nil {
			result.ClientDisconnect = true
		} else {
			c.Writer.Flush()
		}
	}
	trace.Outcome = "completed"
	trace.DecisionPhase = "completed"
	return result, nil
}

// consumeDevinChat serializes content and keepalive writes on the request goroutine.
func consumeDevinChat(ctx context.Context, c *gin.Context, client *devin.Client, base, token string, wire *pb.GetChatMessageRequest, decoder *devin.EventDecoder, stream bool, firstToken **int, clientDisconnected *bool, drainInterval time.Duration, started time.Time) error {
	if err := ctx.Err(); err != nil {
		*clientDisconnected = true
		return err
	}
	upstreamCtx, release := detachStreamUpstreamContext(ctx, stream)
	defer release()
	upstreamCtx, cancel := context.WithCancel(upstreamCtx)
	defer cancel()
	requestDone := ctx.Done()
	var drainTimer *time.Timer
	var drainExpired <-chan time.Time
	resetDrainTimer := func() {
		if drainInterval <= 0 {
			drainInterval = 3 * time.Minute
		}
		if drainTimer == nil {
			drainTimer = time.NewTimer(drainInterval)
			drainExpired = drainTimer.C
		} else {
			drainTimer.Reset(drainInterval)
		}
	}
	defer func() {
		if drainTimer != nil {
			drainTimer.Stop()
		}
	}()
	type event struct {
		message *pb.GetChatMessageResponse
		err     error
		done    bool
	}
	events := make(chan event, 1)
	go func() {
		err := client.Chat(upstreamCtx, base, token, wire, func(m *pb.GetChatMessageResponse) error {
			select {
			case events <- event{message: m}:
				return nil
			case <-upstreamCtx.Done():
				return upstreamCtx.Err()
			}
		})
		select {
		case events <- event{err: err, done: true}:
		case <-upstreamCtx.Done():
		}
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-requestDone:
			*clientDisconnected = true
			if !stream {
				return ctx.Err()
			}
			requestDone = nil
			resetDrainTimer()
		case <-drainExpired:
			return errors.New("devin stream data interval timeout after client disconnect")
		case <-tick.C:
			if stream && decoder.Started && !*clientDisconnected {
				if _, err := c.Writer.WriteString(": keepalive\n\n"); err != nil {
					*clientDisconnected = true
					resetDrainTimer()
				} else {
					c.Writer.Flush()
				}
			}
		case e := <-events:
			if e.done {
				return e.err
			}
			m := e.message
			if *firstToken == nil && (m.DeltaText != "" || m.DeltaThinking != "" || len(m.DeltaToolCalls) > 0) {
				ms := int(time.Since(started).Milliseconds())
				*firstToken = &ms
			}
			if err := decoder.Apply(m); err != nil {
				return err
			}
			if *clientDisconnected {
				resetDrainTimer()
			}
		}
	}
}

func (s *OpenAIGatewayService) devinForwardFailure(c *gin.Context, a *Account, err error) error {
	status := 502
	var upstream *devin.Error
	if errors.As(err, &upstream) {
		status = upstream.HTTPStatus()
	}
	if status == 429 && s.accountRepo != nil {
		_ = s.accountRepo.SetRateLimited(c.Request.Context(), a.ID, time.Now().Add(time.Minute))
	}
	if status == 401 && s.accountRepo != nil {
		_ = s.accountRepo.SetError(c.Request.Context(), a.ID, "Devin authorization expired; reconnect this account")
	}
	if status == 400 {
		c.JSON(400, gin.H{"error": gin.H{"type": "invalid_request_error", "message": "Devin rejected the request"}})
		return err
	}
	return &UpstreamFailoverError{StatusCode: status, ResponseBody: []byte(`{"error":{"message":"Devin upstream unavailable"}}`)}
}
