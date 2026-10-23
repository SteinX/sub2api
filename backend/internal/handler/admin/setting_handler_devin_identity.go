package admin

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func (h *SettingHandler) GetDevinClientIdentity(c *gin.Context) {
	view, err := h.settingService.GetDevinClientIdentitySettings(c.Request.Context(), false)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}

func (h *SettingHandler) UpdateDevinClientIdentity(c *gin.Context) {
	var settings *service.DevinClientIdentitySettings
	d := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&settings); err != nil {
		response.BadRequest(c, "Invalid Devin client identity settings: "+err.Error())
		return
	}
	if settings == nil {
		response.BadRequest(c, "Expected a Devin client identity settings object")
		return
	}
	if err := d.Decode(new(any)); err != io.EOF {
		response.BadRequest(c, "Expected one Devin client identity settings object")
		return
	}
	view, err := h.settingService.UpdateDevinClientIdentitySettings(c.Request.Context(), *settings)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}

func (h *SettingHandler) SyncDevinClientIdentity(c *gin.Context) {
	view, err := h.settingService.GetDevinClientIdentitySettings(c.Request.Context(), true)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, view)
}
