// Package contextcompact implements the deterministic parts of historical
// context compaction. Callers own inference, authorization, billing, storage
// and the atomic commit of a validated replacement.
package contextcompact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

type Lane string

const (
	Main     Lane = "main"
	Lead     Lane = "lead"
	Sidekick Lane = "sidekick"
)

// Item is the gateway's normalized message/tool representation. Provider-only
// reasoning is opaque; it is never included in a summary request.
type Item = apicompat.ResponsesInputItem

// Error has no transport or provider dependency. Adapters may translate its
// code to their existing API error namespace without changing persisted data.
type Error struct {
	Code    string
	Message string
	Status  int
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Digest preserves the existing canonical JSON identity used by Fusion.
// Inputs are normalized, JSON-serializable gateway values.
func Digest(v any) string {
	b, _ := json.Marshal(v)
	var canonical any
	if json.Unmarshal(b, &canonical) == nil {
		b, _ = json.Marshal(canonical)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type Snapshot struct {
	Scope              string
	SessionID          string
	Lane               Lane
	BranchID           string
	Epoch              uint64
	Revision           string
	Policy             string
	ModelRevision      string
	ContractRevision   string
	VisibilityRevision string
	PrefixCount        int
	PrefixDigest       string
}

// Matches accepts append-only suffix growth, but never a changed source prefix
// or another lane, branch, model, contract, policy or context generation.
// Callers must still check cancellation/pending tools and commit with CAS.
func (s Snapshot) Matches(current Snapshot, items []Item) bool {
	return s.Scope == current.Scope && s.SessionID == current.SessionID &&
		s.Lane == current.Lane && s.BranchID == current.BranchID &&
		s.Epoch == current.Epoch && s.Revision == current.Revision &&
		s.Policy == current.Policy && s.ModelRevision == current.ModelRevision &&
		s.ContractRevision == current.ContractRevision && s.VisibilityRevision == current.VisibilityRevision &&
		s.PrefixCount > 0 && s.PrefixCount <= len(items) &&
		Digest(items[:s.PrefixCount]) == s.PrefixDigest
}
