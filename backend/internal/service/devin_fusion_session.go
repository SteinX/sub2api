package service

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/fusion"
	"github.com/gin-gonic/gin"
)

const devinFusionOMPSessionHeader = "X-Litellm-Session-Id"

// devinFusionRequestSessionID adopts runtime client identity only after the
// caller has established Fusion intent. Ordinary routing keeps its own rules.
func devinFusionRequestSessionID(c *gin.Context) (string, error) {
	if explicit := c.GetHeader(fusion.HeaderSession); explicit != "" {
		// A returned Fusion ID overrides the client's original conversation ID;
		// Resolve validates it and still checks authenticated protocol lineage.
		return explicit, nil
	}
	clientSession := ExtractClientSessionID(c)
	values := c.Request.Header.Values(devinFusionOMPSessionHeader)
	ompSession := ""
	if len(values) > 0 {
		if len(values) != 1 {
			return "", fusion.E("session_invalid", "OMP must send one runtime session ID", 400)
		}
		raw := values[0]
		if len(raw) > maxPersistedSessionIDLength || !utf8.ValidString(raw) || strings.ContainsFunc(raw, unicode.IsControl) {
			return "", fusion.E("session_invalid", "OMP session ID must be valid UTF-8, at most 255 bytes and contain no control characters", 400)
		}
		ompSession = strings.TrimSpace(raw)
		if ompSession == "" {
			return "", fusion.E("session_invalid", "OMP runtime session ID must not be empty", 400)
		}
	}
	if clientSession != "" && ompSession != "" && clientSession != ompSession {
		return "", fusion.E("session_conflict", "client session headers identify different conversations; send X-Sub2API-Fusion-Session explicitly", 409)
	}
	if clientSession == "" {
		clientSession = ompSession
	}
	if clientSession == "" {
		return "", nil
	}
	return "client_" + fusion.Digest(clientSession)[:48], nil
}
