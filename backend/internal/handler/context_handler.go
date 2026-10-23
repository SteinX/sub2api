package handler

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	cc "github.com/Wei-Shaw/sub2api/internal/pkg/contextcompact"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
	middleware2 "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *OpenAIGatewayHandler) contextError(c *gin.Context, err error) {
	var typed *cc.Error
	if errors.Is(err, mc.ErrNotFound) {
		typed = mc.E("not_found", "context resource is unavailable in this API key scope", 404)
	} else if !errors.As(err, &typed) {
		typed = mc.E("unavailable", "context operation could not complete; retry with the same idempotency key", 503)
	}
	if !strings.HasPrefix(typed.Code, "context_") {
		typed = mc.E(typed.Code, typed.Message, typed.Status)
	}
	c.Set(service.ResponseCommittedKey, true)
	c.JSON(typed.Status, gin.H{"error": gin.H{"type": "invalid_request_error", "code": typed.Code, "message": typed.Message}})
}
func writeContextRunHeaders(c *gin.Context, result *service.ContextRunResult) {
	if result == nil {
		return
	}
	if result.SessionID != "" {
		c.Header(mc.HeaderSession, result.SessionID)
	}
	if result.Revision > 0 {
		c.Header("X-Sub2API-Context-Revision", strconv.FormatInt(result.Revision, 10))
	}
	if result.Replayed {
		c.Header("X-Sub2API-Context-Replayed", "true")
	}
	if result.Compact != nil {
		c.Header("X-Sub2API-Context-Compaction", result.Compact.ArtifactID)
	} else if result.Job != nil {
		c.Header("X-Sub2API-Context-Compaction", result.Job.ID)
	}
}

func (h *OpenAIGatewayHandler) contextResponse(c *gin.Context, key *service.APIKey, body []byte) {
	result, err := h.gatewayService.RunContext(c, key, h.apiKeyService, body, "responses", c.GetHeader(mc.HeaderSession), 0, 0)
	writeContextRunHeaders(c, result)
	if err != nil {
		h.contextError(c, err)
		return
	}
	if result == nil || result.Response == nil {
		h.contextError(c, mc.E("unavailable", "context returned no response", 503))
		return
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)
	if err = service.WriteContextResponse(c, result.Response, request.Stream); err != nil {
		_ = c.Error(err)
	}
}

// ContextCompact is a platform API independent of /responses/compact.
func (h *OpenAIGatewayHandler) ContextCompact(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	if h.gatewayService == nil {
		h.contextError(c, mc.E("unavailable", "context service is unavailable", 503))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 2<<20)
	body, err := readLenientJSONRequestBodyWithPrealloc(c.Request, h.cfg)
	if err != nil {
		h.contextError(c, mc.E("input_invalid", "unable to read bounded compact request", boundedContextStatus(err)))
		return
	}
	var request struct {
		Mode             string `json:"mode"`
		SessionID        string `json:"session_id"`
		Lane             string `json:"lane"`
		ExpectedRevision int64  `json:"expected_revision"`
		Target           int    `json:"target_input_tokens"`
	}
	if json.Unmarshal(body, &request) != nil {
		h.contextError(c, mc.E("input_invalid", "invalid compact request", 400))
		return
	}
	if request.Mode != "stateless" && request.Mode != "internal" {
		h.contextError(c, mc.E("input_invalid", "mode must be stateless or internal", 400))
		return
	}
	if request.Mode == "internal" && (request.SessionID == "" || request.ExpectedRevision < 1 || request.Lane != "main") {
		h.contextError(c, mc.E("input_invalid", "internal compact requires session_id, lane=main and expected_revision", 400))
		return
	}
	// Apply the same configured content audit to supplied history. Session-only
	// control operations have no new model-visible input to audit.
	if request.Mode == "stateless" {
		subject, ok := middleware2.GetAuthSubjectFromContext(c)
		if !ok {
			h.contextError(c, mc.E("unavailable", "user context is unavailable", 503))
			return
		}
		var r apicompat.ResponsesRequest
		if json.Unmarshal(body, &r) != nil {
			h.contextError(c, mc.E("input_invalid", "invalid compact body", 400))
			return
		}
		log := requestLogger(c, "handler.context.compact")
		if decision := h.checkSecurityAudit(c, log, key, subject, service.ContentModerationProtocolOpenAIResponses, r.Model, body); decision != nil && !decision.AllowNextStage {
			h.openAISecurityAuditError(c, decision)
			return
		}
	}
	result, err := h.gatewayService.RunContext(c, key, h.apiKeyService, body, request.Mode, request.SessionID, request.ExpectedRevision, request.Target)
	writeContextRunHeaders(c, result)
	if err != nil {
		h.contextError(c, err)
		return
	}
	if result.Compact != nil {
		c.JSON(http.StatusOK, result.Compact)
		return
	}
	if result.Job != nil {
		c.JSON(http.StatusOK, gin.H{"job_id": result.Job.ID, "status": result.Job.State, "session_id": result.SessionID, "applied": result.Job.State == "applied"})
		return
	}
	h.contextError(c, mc.E("unavailable", "compact operation returned no result", 503))
}
func (h *OpenAIGatewayHandler) ContextSession(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	engine, err := h.gatewayService.ContextEngine()
	if err != nil {
		h.contextError(c, err)
		return
	}
	scope := service.ContextScope(key)
	s, r, err := engine.LoadSession(c.Request.Context(), scope, c.Param("id"))
	if err != nil {
		h.contextError(c, err)
		return
	}
	if c.Request.Method == http.MethodPost {
		if err = engine.Cancel(c.Request.Context(), scope, s.ID); err != nil {
			h.contextError(c, err)
			return
		}
		c.JSON(200, gin.H{"id": s.ID, "status": "cancelled"})
		return
	}
	if err = h.gatewayService.CheckContextModelAccess(key, s.Model); err != nil {
		h.contextError(c, err)
		return
	}
	if !s.ExpiresAt.After(time.Now()) {
		h.contextError(c, mc.E("expired", "managed session expired", 410))
		return
	}
	switch {
	case strings.HasSuffix(c.FullPath(), "/history/:message_id"):
		offset, parseErr := strconv.Atoi(c.DefaultQuery("offset", "0"))
		if parseErr != nil || offset < 0 {
			h.contextError(c, mc.E("input_invalid", "offset must be a nonnegative integer", 400))
			return
		}
		data, total, digest, err := engine.Store.Chunk(c.Request.Context(), scope, s.ID, c.Param("message_id"), offset, 256<<10)
		if err != nil {
			h.contextError(c, err)
			return
		}
		c.JSON(200, gin.H{"id": c.Param("message_id"), "encoding": "base64", "data": base64.StdEncoding.EncodeToString(data), "offset": offset, "next_offset": offset + len(data), "total_bytes": total, "sha256": digest})
	case strings.HasSuffix(c.FullPath(), "/history"):
		cursor, cursorErr := strconv.ParseInt(c.DefaultQuery("cursor", "0"), 10, 64)
		limit, limitErr := strconv.Atoi(c.DefaultQuery("limit", "20"))
		if cursorErr != nil || limitErr != nil || cursor < 0 || limit < 1 || limit > 50 {
			h.contextError(c, mc.E("input_invalid", "cursor must be nonnegative and limit must be 1..50", 400))
			return
		}
		entries, err := engine.Store.History(c.Request.Context(), scope, s.ID, cursor, limit)
		if err != nil {
			h.contextError(c, err)
			return
		}
		next := cursor
		if len(entries) > 0 {
			next = entries[len(entries)-1].Sequence
		}
		c.JSON(200, gin.H{"data": entries, "next_cursor": next})
	default:
		stats, countErr := engine.Store.UsageStats(c.Request.Context(), scope, s.ID)
		if countErr != nil {
			h.contextError(c, countErr)
			return
		}
		var budget any
		if s.LastBudget != nil {
			budget = gin.H{"estimated_input_tokens": s.LastBudget.InputEstimate, "safe_input_tokens": s.LastBudget.SafeInput, "estimate_method": s.LastBudget.Method}
		}
		c.JSON(200, gin.H{"context_budget": budget, "pending_settlements": stats.Known, "unknown_attempts": stats.Unknown, "id": s.ID, "model": s.Model, "status": s.Status, "revision": r.Revision, "epoch": s.Epoch, "previous_response_id": s.Head, "expires_at": s.ExpiresAt, "history_bytes": s.HistoryBytes, "reserved_bytes": s.ReservedBytes, "requests": s.Requests, "inference_usage": stats.Inference, "maintenance_usage": stats.Maintenance, "maintenance_cost_usd": stats.MaintenanceCostUSD, "compaction_job_id": s.JobID})
	}
}
func (h *OpenAIGatewayHandler) ContextJob(c *gin.Context) {
	key, ok := middleware2.GetAPIKeyFromContext(c)
	if !ok {
		h.errorResponse(c, 401, "authentication_error", "Invalid API key")
		return
	}
	engine, err := h.gatewayService.ContextEngine()
	if err != nil {
		h.contextError(c, err)
		return
	}
	j, r, err := engine.LoadJob(c.Request.Context(), service.ContextScope(key), c.Param("job_id"))
	if err != nil {
		h.contextError(c, err)
		return
	}
	if err = h.gatewayService.CheckContextModelAccess(key, j.Model); err != nil {
		h.contextError(c, err)
		return
	}
	if !r.ExpiresAt.After(time.Now()) {
		h.contextError(c, mc.E("expired", "compact job expired", 410))
		return
	}
	var usage any = j.Usage
	settled := j.Accounted
	if j.AttemptID != "" {
		if ar, readErr := engine.Store.Read(c.Request.Context(), service.ContextScope(key), "attempt", j.AttemptID); readErr == nil {
			var a mc.Attempt
			if json.Unmarshal(ar.Data, &a) == nil {
				if j.Usage == nil {
					usage = a.Usage
				}
				settled = a.Settled
			}
		}
	}
	c.JSON(200, gin.H{"usage": usage, "id": j.ID, "status": j.State, "session_id": j.SessionID, "trigger": j.Trigger, "applied": j.State == "applied", "before": j.Before, "after": j.After, "reason": j.Reason, "estimate_method": cc.ConservativeUTF8V1, "usage_settled": settled})
}

func boundedContextStatus(err error) int {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
