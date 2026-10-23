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
	ctx := devin.WithClientIdentity(c.Request.Context())
	requestedModel := model
	if err := devin.CheckModelIntent(nil, model); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	d := s.devinClientService()
	models, err := d.Models(ctx, a)
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if len(models) == 0 {
		return s.sendErrorAndEnd(c, "Devin returned no available models")
	}
	if model == "" {
		available := devin.AvailableModelIDs(models)
		if len(available) == 0 {
			return s.sendErrorAndEnd(c, "Devin returned no supported model routes")
		}
		model = available[0]
	}
	model = a.GetMappedModel(model)
	if err := devin.CheckModelIntent(models, requestedModel, model); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
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
	plan := devin.PlanModel(selected)
	if err := plan.Validate(); err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if prompt == "" {
		prompt = "Reply with OK."
	}
	raw, _ := json.Marshal(prompt)
	// Use the same native defaults and discovered model limits as real traffic.
	// A fixed tiny output budget can be invalid for reasoning-enabled models.
	request := &apicompat.ResponsesRequest{Model: model, Input: raw}
	q, err := devin.BuildRequest(request, fmt.Sprintf("account-test:%d", a.ID))
	if err != nil {
		return s.sendErrorAndEnd(c, err.Error())
	}
	if err = devin.ApplyModelConfig(q, selected, request); err != nil {
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
	announced := false
	decoder := devin.NewEventDecoder(model, func(apicompat.ResponsesStreamEvent) error { return nil })
	for attempt := 0; attempt < 2; attempt++ {
		q.ChatModelUid = model
		q.ModelAssignmentJwt = nil
		var prepared *devin.PrepareResult
		prepared, err = client.Prepare(ctx, token, q, plan, models)
		if err == nil {
			err = devin.ApplyModelConfig(q, devin.FindModel(models, prepared.FinalModelUID), request)
		}
		if err == nil {
			if !announced {
				s.sendEvent(c, TestEvent{Type: "test_start", Model: model})
				announced = true
			}
			err = client.Chat(ctx, prepared.BaseURL, token, q, func(m *pb.GetChatMessageResponse) error {
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
