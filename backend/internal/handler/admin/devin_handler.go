package admin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type DevinHandler struct {
	devin *service.DevinService
	admin service.AdminService
	repo  service.AccountRepository
}

func NewDevinHandler(d *service.DevinService, a service.AdminService, repo service.AccountRepository) *DevinHandler {
	return &DevinHandler{d, a, repo}
}
func devinOwner(c *gin.Context) int64 {
	subject, ok := middleware.GetAuthSubjectFromContext(c)
	if !ok {
		return -2
	}
	return subject.UserID
}
func (h *DevinHandler) checkTarget(ctx context.Context, id int64) error {
	if id == 0 {
		return nil
	}
	a, err := h.admin.GetAccount(ctx, id)
	if err != nil {
		return err
	}
	if a.Platform != service.PlatformDevin {
		return errors.New("target account is not a Devin account")
	}
	return nil
}
func (h *DevinHandler) Start(c *gin.Context) {
	var in struct {
		Mode      string `json:"mode"`
		Content   string `json:"content"`
		ProxyID   *int64 `json:"proxy_id"`
		AccountID int64  `json:"account_id"`
	}
	if c.ShouldBindJSON(&in) != nil {
		response.BadRequest(c, "Invalid Devin connection request")
		return
	}
	if err := h.checkTarget(c.Request.Context(), in.AccountID); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	var out service.DevinAuthView
	var err error
	if in.Mode == "browser" {
		out, err = h.devin.StartAuth(c.Request.Context(), devinOwner(c), in.ProxyID, in.AccountID)
	} else if in.Mode == "key" || in.Mode == "import" {
		out, err = h.devin.ImportAuth(c.Request.Context(), devinOwner(c), in.Content, in.ProxyID, in.AccountID)
	} else {
		response.BadRequest(c, "Unknown Devin connection mode")
		return
	}
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, out)
}
func (h *DevinHandler) Status(c *gin.Context) {
	v, err := h.devin.LoadAuth(c.Request.Context(), c.Param("id"), devinOwner(c))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, service.DevinAuthSummary(v))
}
func (h *DevinHandler) Complete(c *gin.Context) { h.complete(c, devinOwner(c)) }
func (h *DevinHandler) Callback(c *gin.Context) { h.complete(c, -1) }
func (h *DevinHandler) complete(c *gin.Context, owner int64) {
	var in struct {
		Code   string `json:"code"`
		State  string `json:"state"`
		Ticket string `json:"pairing_ticket"`
	}
	if c.ShouldBindJSON(&in) != nil {
		response.BadRequest(c, "Invalid callback")
		return
	}
	out, err := h.devin.CompleteAuth(c.Request.Context(), c.Param("id"), in.Code, in.State, in.Ticket, owner)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, out)
}
func (h *DevinHandler) Cancel(c *gin.Context) {
	if err := h.devin.CancelAuth(c.Request.Context(), c.Param("id"), devinOwner(c)); err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	response.Success(c, gin.H{"cancelled": true})
}
func (h *DevinHandler) Commit(c *gin.Context) {
	ctx := c.Request.Context()
	unlock, err := h.devin.AuthLock(ctx, c.Param("id"))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	defer unlock()
	v, err := h.devin.LoadAuth(ctx, c.Param("id"), devinOwner(c))
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	if v.Status == "connected" {
		response.Success(c, service.DevinAuthSummary(v))
		return
	}
	if v.Status != "ready" {
		response.BadRequest(c, "Complete Devin authorization first")
		return
	}
	var in struct {
		Name           string   `json:"name"`
		Notes          *string  `json:"notes"`
		GroupIDs       []int64  `json:"group_ids"`
		Concurrency    int      `json:"concurrency"`
		Priority       int      `json:"priority"`
		RateMultiplier *float64 `json:"rate_multiplier"`
	}
	if c.ShouldBindJSON(&in) != nil {
		response.BadRequest(c, "Invalid account settings")
		return
	}
	if in.Concurrency <= 0 {
		in.Concurrency = 1
	}
	if in.Name == "" {
		in.Name = "Devin"
	}
	// Use a credential fingerprint for duplicates without exposing raw credentials.
	b, _ := json.Marshal(map[string]any{"api_key": v.Credentials["api_key"], "access_token": v.Credentials["access_token"]})
	fingerprint := devin.CredentialFingerprint(string(b))
	credentialUnlock, err := h.devin.AuthLock(ctx, "credential:"+fingerprint)
	if err != nil {
		response.BadRequest(c, err.Error())
		return
	}
	defer credentialUnlock()
	matches, err := h.repo.FindByExtraField(ctx, "devin_credential_id", fingerprint)
	if err != nil {
		response.BadRequest(c, "Could not check existing Devin accounts")
		return
	}
	for _, a := range matches {
		if a.Platform == service.PlatformDevin && a.ID != v.TargetAccountID {
			if a.Extra["devin_auth_session_id"] == v.ID {
				v.Status = "connected"
				v.AccountID = a.ID
				v.Credentials = nil
				if err = h.devin.SaveAuth(ctx, v); err != nil {
					response.BadRequest(c, "Account already saved; reload the account list")
					return
				}
				response.Success(c, service.DevinAuthSummary(v))
				return
			}
			response.BadRequest(c, "This credential is already connected; reconnect the existing account #"+strconv.FormatInt(a.ID, 10))
			return
		}
	}
	var account *service.Account
	kind := service.AccountTypeOAuth
	if v.Credentials["api_key"] != nil {
		kind = service.AccountTypeAPIKey
	}
	extra := map[string]any{"devin_credential_id": fingerprint, "devin_auth_source": v.Credentials["auth_mode"], "devin_models": v.Models, "devin_auth_session_id": v.ID}
	if v.TargetAccountID != 0 {
		old, loadErr := h.admin.GetAccount(ctx, v.TargetAccountID)
		if loadErr != nil {
			response.ErrorFrom(c, loadErr)
			return
		}
		if old.Platform != service.PlatformDevin {
			response.BadRequest(c, "Target is not a Devin account")
			return
		}
		mergedExtra := service.MergeCredentials(old.Extra, extra)
		creds := service.MergeCredentials(old.Credentials, v.Credentials)
		if kind == service.AccountTypeOAuth {
			creds["api_key"] = ""
		} else {
			creds["access_token"] = ""
		}
		// Reauthorization changes credentials only; group, quota and billing settings stay attached.
		account, err = h.admin.UpdateAccount(ctx, v.TargetAccountID, &service.UpdateAccountInput{Type: kind, Credentials: creds, Extra: mergedExtra})
		if err == nil && old.Status == service.StatusError {
			account, err = h.admin.ClearAccountError(ctx, v.TargetAccountID)
		}
	} else {
		account, err = h.admin.CreateAccount(ctx, &service.CreateAccountInput{Name: in.Name, Notes: in.Notes, Platform: service.PlatformDevin, Type: kind, Credentials: v.Credentials, Extra: extra, ProxyID: v.ProxyID, Concurrency: in.Concurrency, Priority: in.Priority, RateMultiplier: in.RateMultiplier, GroupIDs: in.GroupIDs})
	}
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	v.Status = "connected"
	v.AccountID = account.ID
	v.Credentials = nil
	if err = h.devin.SaveAuth(ctx, v); err != nil {
		response.BadRequest(c, "Account saved; reload the account list before retrying")
		return
	}
	response.Success(c, service.DevinAuthSummary(v))
}
