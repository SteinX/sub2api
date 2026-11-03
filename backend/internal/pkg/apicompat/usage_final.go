package apicompat

// HasFinalUsage distinguishes cumulative usage observed during a stream from a
// final accounting receipt. New provider adapters must supply explicit evidence
// from their transport, independently of output validation. A nil flag supports
// older receipts whose decoder only finalized status after a clean transport.
func HasFinalUsage(response *ResponsesResponse, final *bool) bool {
	if response == nil || response.Usage == nil {
		return false
	}
	if final != nil {
		return *final
	}
	return response.Status == "completed" || response.Status == "incomplete"
}
