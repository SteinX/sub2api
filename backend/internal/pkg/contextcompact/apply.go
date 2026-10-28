package contextcompact

type Replacement struct {
	Items       []Item
	BeforeBytes int
	AfterBytes  int
}

func (r Replacement) Smaller() bool { return r.AfterBytes < r.BeforeBytes }

// ReplacePrefix creates a candidate view without changing the original slice.
// It deliberately does not commit: the caller must verify the final provider
// request budget, source snapshot and execution ownership in its transaction.
func ReplacePrefix(items []Item, count int, protected []Item, summary Item) (Replacement, error) {
	if count < 1 || count > len(items) {
		return Replacement{}, &Error{"compaction_failed", "summary source prefix is invalid", 502}
	}
	next := make([]Item, 0, len(protected)+1+len(items)-count)
	next = append(next, protected...)
	next = append(next, summary)
	next = append(next, items[count:]...)
	return Replacement{Items: next, BeforeBytes: SizeBytes(items), AfterBytes: SizeBytes(next)}, nil
}
