//go:build unit

package service

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/devin"
	"github.com/stretchr/testify/require"
)

func TestDevinFusionFailureKindRedactsNativeDetails(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{nil, "none"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "deadline_exceeded"},
		{context.Canceled, "cancelled"},
		{&devin.Error{Code: "permission_denied", TraceID: "private-trace"}, "native_permission_denied"},
		{&devin.Error{Code: "private-untrusted-code", TraceID: "private-trace"}, "native_error"},
		{fmt.Errorf("wrapper: %w", errors.New("devin returned incomplete tool arguments")), "incomplete_tool_arguments"},
		{errors.New("devin tool call has no name"), "tool_call_missing_name"},
		{errors.New("devin tool argument delta has no call"), "tool_delta_missing_call"},
		{errors.New("devin returned no content"), "empty_response"},
		{errors.New("untrusted error with private prompt or credential"), "transport_or_decode_error"},
	} {
		require.Equal(t, test.want, devinFusionFailureKind(test.err))
	}
}
