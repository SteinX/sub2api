package service

import (
	"encoding/json"
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
	"github.com/gin-gonic/gin"
)

func (s *AccountTestService) devinClientService() *DevinService {
	if s.devin != nil {
		return s.devin
	}
	return NewDevinService(s.accountRepo, nil, s.httpUpstream, s.cfg, nil)
}

func (s *AccountTestService) testDevinAccountConnection(c *gin.Context, a *Account, model, prompt string) error {
	ctx := c.Request.Context()
	d := s.devinClientService()
	models, err := d.Models(ctx, a)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if len(models) == 0 {
		return s.sendErrorAndEnd(c, "Devin returned no available models")
	}
	if model == "" {
		model = devin.ModelID(models[0])
	}
	model = a.GetMappedModel(model)
	var selected *pb.ClientModelConfig
	for _, m := range models {
		if devin.ModelID(m) == model {
			selected = m
			break
		}
	}
	if selected == nil {
		return s.sendErrorAndEnd(c, "The selected model is not available to this Devin account")
	}
	if prompt == "" {
		prompt = "Reply with OK."
	}
	raw, _ := json.Marshal(prompt)
	limit := 128
	q, err := devin.BuildRequest(&apicompat.ResponsesRequest{Model: model, Input: raw, MaxOutputTokens: &limit}, fmt.Sprintf("account-test:%d", a.ID))
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	client, err := d.Client(ctx, a)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	token, err := d.Token(ctx, a, false)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	c.Header("Content-Type", "text/event-stream")
	s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
	decoder := devin.NewEventDecoder(model, func(apicompat.ResponsesStreamEvent) error { return nil })
	for attempt := 0; attempt < 2; attempt++ {
		q.ChatModelUid = model
		q.ModelAssignmentJwt = nil
		var base string
		base, err = client.Prepare(ctx, token, q, selected.GetModelInfo().GetIsModelRouter())
		if err == nil {
			err = client.Chat(ctx, base, token, q, func(m *pb.GetChatMessageResponse) error {
				if m.DeltaText != "" {
					s.sendEvent(c, TestEvent{Type: "content", Text: m.DeltaText})
				}
				return decoder.Apply(m)
			})
		}
		if err == nil {
			err = decoder.Finish()
		}
		if ctx.Err() != nil || !devin.IsUnauthenticated(err) || decoder.Started || attempt > 0 || a.GetCredential("api_key") == "" {
			break
		}
		token, err = d.RecoverToken(ctx, a, token)
		if err != nil {
			break
		}
	}
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}
