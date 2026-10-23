package contextcompact

import (
	"encoding/json"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
)

// ConservativeUTF8V1 counts serialized UTF-8 bytes as possible input tokens.
// It is an estimate, not a provider-independent tokenizer. Wire and storage
// byte limits remain separate from the concrete model's input allowance.
const ConservativeUTF8V1 = "conservative_utf8_v1"

type Budget struct {
	InputEstimate int
	SafeInput     int
	Method        string
}

// MeasureRequest may replace the normalized byte estimate with the actual
// provider request shape and reduce the safe input budget (e.g. output reserve).
type MeasureRequest func(*apicompat.ResponsesRequest) (size, safe int, err error)

func Measure(req *apicompat.ResponsesRequest, maxInput int, provider MeasureRequest) (Budget, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return Budget{}, err
	}
	budget := Budget{InputEstimate: len(b), SafeInput: maxInput, Method: ConservativeUTF8V1}
	if provider != nil {
		budget.InputEstimate, budget.SafeInput, err = provider(req)
		if err != nil {
			return Budget{}, err
		}
		budget.SafeInput = min(budget.SafeInput, maxInput)
	}
	if budget.SafeInput <= 0 {
		return Budget{}, &Error{"context_limit", "fixed context and output exceed the concrete model window", 413}
	}
	return budget, nil
}

type Thresholds struct {
	Spawn int
	Apply int
	Hard  int
}

// ValidateThresholds validates explicitly configured conservative input units.
// Empty values select budget ratios; two values mean spawn,hard (apply=hard).
func ValidateThresholds(values []int, minimum, maximum int) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) != 2 && len(values) != 3 {
		return &Error{"compaction_invalid", "thresholds must be spawn,hard or spawn,apply,hard", 400}
	}
	for i, n := range values {
		if n < minimum || n > maximum || i > 0 && n < values[i-1] {
			return &Error{"compaction_invalid", "compaction thresholds must be positive and ordered", 400}
		}
	}
	return nil
}

func ResolveThresholds(values []int, safe int) (Thresholds, error) {
	t := Thresholds{safe * 60 / 100, safe * 75 / 100, safe * 90 / 100}
	if len(values) == 2 {
		t = Thresholds{values[0], values[1], values[1]}
	} else if len(values) == 3 {
		t = Thresholds{values[0], values[1], values[2]}
	}
	if t.Spawn <= 0 || t.Spawn > t.Apply || t.Apply > t.Hard || t.Hard > safe {
		return t, &Error{"context_limit", "compaction thresholds exceed the concrete model's safe input budget", 413}
	}
	return t, nil
}
