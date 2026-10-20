package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
)

func (s *OpenAIGatewayService) SetDevinService(d *DevinService) { s.devin = d }

func (s *OpenAIGatewayService) forwardDevin(ctx context.Context, c *gin.Context, a *Account, body []byte, protocol, cacheKey, mapped string) (*OpenAIForwardResult, error) {
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
	var selected *pb.ClientModelConfig
	for _, m := range models {
		if devin.ModelID(m) == req.Model {
			selected = m
			break
		}
	}
	if selected == nil {
		writeError(400, "This Devin account does not advertise the requested model")
		return nil, fmt.Errorf("devin model is unavailable")
	}
	if limit := selected.GetModelInfo().GetMaxOutputTokens(); limit > 0 {
		if req.MaxOutputTokens != nil && *req.MaxOutputTokens > int(limit) {
			writeError(400, "max_output_tokens exceeds this Devin model's limit")
			return nil, fmt.Errorf("devin output token limit exceeded")
		}
		if wire.Configuration.MaxTokens > uint64(limit) {
			wire.Configuration.MaxTokens = uint64(limit)
		}
	}
	for _, p := range wire.ChatMessagePrompts {
		if len(p.Images) > 0 && !selected.SupportsImages && !selected.GetModelInfo().GetModelFeatures().GetSupportsImages() {
			writeError(400, "The selected Devin model does not support images")
			return nil, fmt.Errorf("devin model has no image support")
		}
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
	seq := 0
	emit := func(e apicompat.ResponsesStreamEvent) error {
		if !req.Stream {
			return nil
		}
		if !c.Writer.Written() {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("X-Accel-Buffering", "no")
		}
		write := func(text string) error {
			_, err := c.Writer.Write([]byte(text))
			if err == nil {
				c.Writer.Flush()
			}
			return err
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
	for attempt := 0; attempt < 2; attempt++ {
		wire.ChatModelUid = req.Model
		wire.ModelAssignmentJwt = nil
		base, prepErr := client.Prepare(ctx, token, wire, selected.GetModelInfo().GetIsModelRouter())
		err = prepErr
		if err == nil {
			err = consumeDevinChat(ctx, c, client, base, token, wire, decoder, req.Stream, &firstToken, started)
		}
		if err == nil {
			err = decoder.Finish()
		}
		if !devin.IsUnauthenticated(err) || decoder.Started || attempt > 0 || a.GetCredential("api_key") == "" {
			break
		}
		token, err = s.devin.RecoverToken(ctx, a, token)
		if err != nil {
			break
		}
	}
	billingModel := decoder.BillingModel
	if billingModel == "" {
		billingModel = req.Model
	}
	result := &OpenAIForwardResult{Model: original, BillingModel: billingModel, UpstreamModel: wire.ChatModelUid, UpstreamResponseModel: decoder.ActualModel, ResponseID: decoder.Response.ID, Stream: req.Stream, Duration: time.Since(started), FirstTokenMs: firstToken, UpstreamEndpoint: devin.ChatPath}
	if u := decoder.Response.Usage; u != nil {
		result.Usage = OpenAIUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheCreationInputTokens: u.CacheCreationInputTokens}
		if u.InputTokensDetails != nil {
			result.Usage.CacheReadInputTokens = u.InputTokensDetails.CachedTokens
		}
	}
	if err != nil {
		if !c.Writer.Written() {
			if decoder.Started {
				writeError(502, "Devin generation did not complete")
				return result, err
			}
			return nil, s.devinForwardFailure(c, a, err)
		}
		switch protocol {
		case "anthropic":
			b, _ := json.Marshal(gin.H{"type": "error", "error": gin.H{"type": "api_error", "message": "Devin stream failed"}})
			_, _ = fmt.Fprintf(c.Writer, "event: error\ndata: %s\n\n", b)
		case "chat":
			b, _ := json.Marshal(gin.H{"error": gin.H{"type": "upstream_error", "message": "Devin stream failed"}})
			_, _ = fmt.Fprintf(c.Writer, "data: %s\n\n", b)
		default:
			_ = emit(apicompat.ResponsesStreamEvent{Type: "response.failed", Response: &apicompat.ResponsesResponse{ID: decoder.Response.ID, Object: "response", CreatedAt: decoder.Response.CreatedAt, Model: original, Status: "failed", Output: decoder.Response.Output, Error: &apicompat.ResponsesError{Code: "upstream_error", Message: "Devin stream failed"}}})
		}
		c.Writer.Flush()
		result.ClientDisconnect = ctx.Err() != nil
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
		_, _ = c.Writer.WriteString("data: [DONE]\n\n")
		c.Writer.Flush()
	}
	return result, nil
}

// consumeDevinChat serializes content and keepalive writes on the request goroutine.
func consumeDevinChat(ctx context.Context, c *gin.Context, client *devin.Client, base, token string, wire *pb.GetChatMessageRequest, decoder *devin.EventDecoder, stream bool, firstToken **int, started time.Time) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type event struct {
		message *pb.GetChatMessageResponse
		err     error
		done    bool
	}
	events := make(chan event, 1)
	go func() {
		err := client.Chat(ctx, base, token, wire, func(m *pb.GetChatMessageResponse) error {
			select {
			case events <- event{message: m}:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
		select {
		case events <- event{err: err, done: true}:
		case <-ctx.Done():
		}
	}()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			if stream && decoder.Started {
				if _, err := c.Writer.WriteString(": keepalive\n\n"); err != nil {
					return err
				}
				c.Writer.Flush()
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
