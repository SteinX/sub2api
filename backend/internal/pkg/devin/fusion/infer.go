package fusion

import (
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/devin/pb"
)

const RouteMode = "gateway_fusion"
const ExtraEnabled = "devin_fusion_enabled"

// DefaultProfile applies bounded gateway policy to the pair encoded in a model
// ID. It is not a claim that the official private harness uses this policy.
func DefaultProfile(public, lead, sidekick string) Profile {
	return Profile{Enabled: true, PublicModel: public, LeadModel: lead, SidekickModel: sidekick, MaxHandoffs: 4, MaxRequests: 20, MaxTools: 40, MaxWallSeconds: 900, MaxCostUSD: 5, MaxContextBytes: 262144, MaxOutputTokens: 4096, TTLSeconds: 86400}
}
func normalizeMemberID(id string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(id)))
}

// The native catalog calls some advertised Fast variants "-priority". Resolve
// that spelling only with the exact base/effort and explicit Fast Mode metadata;
// never fall back to the ordinary lane when the Fast variant is unavailable.
func catalogFastAlias(fragment string, m *pb.ClientModelConfig) bool {
	fragment = normalizeMemberID(fragment)
	uid := normalizeMemberID(devin.ModelID(m))
	if !strings.HasSuffix(fragment, "-fast") || !strings.HasSuffix(uid, "-priority") || strings.TrimSuffix(fragment, "-fast") != strings.TrimSuffix(uid, "-priority") {
		return false
	}
	for _, entry := range m.GetModelFamilyMetadata().GetEntries() {
		if entry.GetKey() == "Fast Mode" && entry.GetValue().GetOrder() > 0 {
			return true
		}
	}
	return false
}
func concreteMatches(fragment string, models []*pb.ClientModelConfig) []string {
	exact := map[string]bool{}
	normalized := map[string]bool{}
	for _, m := range models {
		if devin.PlanModel(m).Mode != devin.RouteDirect {
			continue
		}
		if f := m.GetModelInfo().GetModelFeatures(); f != nil && !f.GetSupportsToolCalls() {
			continue
		}
		if catalogFastAlias(fragment, m) {
			normalized[devin.ModelID(m)] = true
		}
		for _, name := range []string{devin.ModelID(m), m.GetModelInfo().GetModelUid(), m.GetModelOrAlias().GetModelUid()} {
			if name == "" {
				continue
			}
			if strings.EqualFold(fragment, name) {
				exact[devin.ModelID(m)] = true
			}
			if normalizeMemberID(fragment) == normalizeMemberID(name) {
				normalized[devin.ModelID(m)] = true
			}
		}
	}
	matches := normalized
	if len(exact) > 0 {
		matches = exact
	}
	out := make([]string, 0, len(matches))
	for id := range matches {
		out = append(out, id)
	}
	return out
}

// InferProfile resolves fusion-<lead>-sidekick-<sidekick> against this account's
// concrete model catalog. Catalog UIDs and their explicit aliases are accepted;
// punctuation normalization is permitted only when it selects a unique member.
// Preserve effort/Fast semantics, never guess a missing member or invoke the public
// entry's opaque harness: the public ID names a gateway-managed virtual pair.
func InferProfile(public string, models []*pb.ClientModelConfig) (*Profile, error) {
	if !strings.HasPrefix(strings.ToLower(public), "fusion-") {
		return nil, E("pair_unresolved", "Fusion model ID does not encode a Lead/Sidekick pair", 400)
	}
	if advertised := devin.FindModel(models, public); advertised != nil && advertised.GetDisabled() {
		return nil, E("pair_unresolved", "Fusion catalog entry is disabled", 400)
	}
	rest := public[len("fusion-"):]
	lower := strings.ToLower(rest)
	pairs := map[string]Profile{}
	for offset := 0; offset < len(lower); {
		relative := strings.Index(lower[offset:], "-sidekick-")
		if relative < 0 {
			break
		}
		split := offset + relative
		lead, worker := rest[:split], rest[split+len("-sidekick-"):]
		if lead != "" && worker != "" {
			leads := concreteMatches(lead, models)
			workers := concreteMatches(worker, models)
			for _, l := range leads {
				for _, w := range workers {
					p := DefaultProfile(public, l, w)
					pairs[l+"\x00"+w] = p
					// Further candidates cannot remove an existing ambiguity. Stop
					// before expanding the remaining alias Cartesian product.
					if len(pairs) > 1 {
						return nil, E("pair_ambiguous", "Fusion model ID matches multiple concrete pairs; configure an explicit override", 400)
					}
				}
			}
		}
		offset = split + 1
	}
	if len(pairs) == 0 {
		return nil, E("pair_unresolved", "Fusion ID members are missing, disabled or not directly callable; sync models or add a pair override", 400)
	}
	for _, p := range pairs {
		if err := p.Validate(); err != nil {
			return nil, err
		}
		return &p, nil
	}
	return nil, E("pair_unresolved", "Fusion pair is unavailable", 400)
}
