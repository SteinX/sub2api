package service

import (
	"slices"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	mc "github.com/Wei-Shaw/sub2api/internal/pkg/managedcontext"
)

const (
	DevinContextEnabledExtra = "devin_context_enabled"
	DevinContextManualExtra  = "devin_context_manual_compact"
	DevinContextAutoExtra    = "devin_context_auto_compaction"
)

var devinContextExtraKeys = []string{DevinContextEnabledExtra, DevinContextManualExtra, DevinContextAutoExtra}

func hasDevinContextExtra(extra map[string]any) bool {
	for _, key := range devinContextExtraKeys {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// Null restores inheritance, including when Extra is updated through a merge.
func ValidateDevinContextExtra(platform string, extra map[string]any) error {
	for _, key := range devinContextExtraKeys {
		value, exists := extra[key]
		if !exists {
			continue
		}
		if platform != PlatformDevin {
			return infraerrors.BadRequest("INVALID_DEVIN_CONTEXT_SETTINGS", "managed context settings require a Devin account")
		}
		if value != nil {
			if _, ok := value.(bool); !ok {
				return infraerrors.BadRequest("INVALID_DEVIN_CONTEXT_SETTINGS", key+" must be boolean or null")
			}
		}
	}
	return nil
}

func ValidateDevinAccountExtra(platform string, extra map[string]any) error {
	if err := ValidateDevinContextExtra(platform, extra); err != nil {
		return err
	}
	return ValidateDevinFusionExtra(platform, extra)
}

type devinContextSettings struct {
	Enabled, Manual, Auto bool
}

func contextBool(extra map[string]any, key string, fallback bool) bool {
	value := extra[key]
	if value == nil {
		return fallback
	}
	// Malformed legacy data fails closed even if it bypassed the save validator.
	enabled, ok := value.(bool)
	return ok && enabled
}

func resolveDevinContextSettings(c config.DevinContextConfig, key *APIKey, account *Account, model string) devinContextSettings {
	if account == nil || account.Platform != PlatformDevin || c.ForceDisabled {
		return devinContextSettings{}
	}
	if account.Extra[DevinContextEnabledExtra] != nil {
		return devinContextSettings{
			Enabled: contextBool(account.Extra, DevinContextEnabledExtra, false),
			Manual:  contextBool(account.Extra, DevinContextManualExtra, true),
			Auto:    contextBool(account.Extra, DevinContextAutoExtra, true),
		}
	}
	// Accounts without an explicit override retain the original, allowlisted rollout.
	return devinContextSettings{
		Enabled: c.Enabled && key != nil && slices.Contains(c.APIKeyIDs, key.ID) && (model == "" || slices.Contains(c.ModelUIDs, model)),
		Manual:  c.ManualCompact,
		Auto:    c.AutoCompaction,
	}
}

func (s *OpenAIGatewayService) checkContextAccountAccess(key *APIKey, account *Account, model string, manual bool) (devinContextSettings, error) {
	if err := s.CheckContextAccess(key, model); err != nil {
		return devinContextSettings{}, err
	}
	c := s.cfg.Gateway.DevinContext
	settings := resolveDevinContextSettings(c, key, account, model)
	if !settings.Enabled || manual && !settings.Manual {
		return settings, mc.E("disabled", "managed context operation is not enabled for this account", 404)
	}
	if len(c.AccountIDs) > 0 && !slices.Contains(c.AccountIDs, account.ID) {
		return settings, mc.E("account_bound", "bound account is outside the context rollout", 409)
	}
	return settings, nil
}
