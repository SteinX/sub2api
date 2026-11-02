//go:build devine2e

package devine2e

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// An expected hard stop can interrupt the asynchronous synthetic usage writer.
// Missing rows are unresolved, never a zero-price or fully-settled assertion.
func mockBillingProof(native, rows, unique, charged int, recorded float64, queryErr error) map[string]any {
	r := map[string]any{"scope": "cumulative pre-matrix synthetic fixture batches; not a provider invoice", "available": queryErr == nil, "expected_native_calls": native, "observed_rows": nil, "unique_rows": nil, "positive_charge_rows": nil, "observed_persisted_local_usd": nil, "full_local_usd": nil, "unresolved_native_calls": nil, "settlement_complete": false}
	if queryErr != nil {
		r["error_code"] = "usage_query_unavailable"
		return r
	}
	r["observed_rows"], r["unique_rows"], r["positive_charge_rows"], r["observed_persisted_local_usd"] = rows, unique, charged, recorded
	complete := rows == native && unique == rows && charged == rows
	r["settlement_complete"] = complete
	if unique <= native {
		r["unresolved_native_calls"] = native - unique
	}
	if complete {
		r["full_local_usd"] = recorded
	}
	return r
}

func snapshotMockBilling(t *testing.T, app *application, fixture *nativeFixture, keyID int64) map[string]any {
	t.Helper()
	fixture.mu.Lock()
	native := len(fixture.chats)
	fixture.mu.Unlock()
	return queryMockBilling(agentDatabase(t, app), keyID, native)
}

func queryMockBilling(db *sql.DB, keyID int64, native int) map[string]any {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var rows, unique, charged int
	var recorded float64
	err := db.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT request_id),count(*) FILTER(WHERE actual_cost>0),coalesce(sum(actual_cost),0) FROM usage_logs WHERE api_key_id=$1`, keyID).Scan(&rows, &unique, &charged, &recorded)
	return mockBillingProof(native, rows, unique, charged, recorded, err)
}

// Each input has a denominator and SQL query for the SAME key/call window.
// Partial or unavailable batches cannot become a fully settled aggregate.
func aggregateMockBilling(batches []map[string]any) map[string]any {
	available, complete := len(batches) > 0, len(batches) > 0
	native, rows, unique, charged := 0, 0, 0, 0
	unresolved, unresolvedKnown := 0, true
	var recorded float64
	for _, batch := range batches {
		count, ok := batch["expected_native_calls"].(int)
		available = available && ok
		native += count
		available = available && batch["available"] == true
		complete = complete && batch["settlement_complete"] == true
		if n, ok := batch["observed_rows"].(int); ok {
			rows += n
		} else {
			available = false
		}
		if n, ok := batch["unique_rows"].(int); ok {
			unique += n
		} else {
			available = false
		}
		if n, ok := batch["positive_charge_rows"].(int); ok {
			charged += n
		} else {
			available = false
		}
		if cost, ok := batch["observed_persisted_local_usd"].(float64); ok {
			recorded += cost
		} else {
			available = false
		}
		if n, ok := batch["unresolved_native_calls"].(int); ok {
			unresolved += n
		} else {
			unresolvedKnown = false
		}
	}
	var err error
	if !available {
		err = fmt.Errorf("one synthetic key query unavailable")
	}
	r := mockBillingProof(native, rows, unique, charged, recorded, err)
	r["scope"] = "all separately queried synthetic fixture keys: original, bounded-long and supported-case batches"
	r["batches"] = len(batches)
	if unresolvedKnown {
		r["unresolved_native_calls"] = unresolved
	} else {
		r["unresolved_native_calls"] = nil
	}
	if !complete {
		r["settlement_complete"] = false
		r["full_local_usd"] = nil
	}
	return r
}

func TestJourneyStopMockBillingAggregateNeverMixesKeyScopes(t *testing.T) {
	a := mockBillingProof(2, 2, 2, 2, .2, nil)
	b := mockBillingProof(3, 3, 3, 3, .3, nil)
	all := aggregateMockBilling([]map[string]any{a, b})
	require.Equal(t, true, all["settlement_complete"])
	require.Equal(t, 5, all["expected_native_calls"])
	require.Equal(t, .5, all["full_local_usd"])
	partial := mockBillingProof(3, 2, 2, 2, .2, nil)
	all = aggregateMockBilling([]map[string]any{a, partial})
	require.Equal(t, false, all["settlement_complete"])
	require.Equal(t, 1, all["unresolved_native_calls"])
	require.Nil(t, all["full_local_usd"])
	unknown := mockBillingProof(1, 0, 0, 0, 0, fmt.Errorf("unavailable"))
	all = aggregateMockBilling([]map[string]any{a, unknown})
	require.Equal(t, false, all["available"])
	require.Nil(t, all["observed_persisted_local_usd"])
	duplicate := mockBillingProof(3, 3, 2, 3, .3, nil)
	all = aggregateMockBilling([]map[string]any{a, duplicate})
	require.Equal(t, false, all["settlement_complete"])
	require.Nil(t, all["full_local_usd"])
	// An overcount on one key cannot cancel a missing row on another key.
	over := mockBillingProof(2, 3, 3, 3, .3, nil)
	all = aggregateMockBilling([]map[string]any{over, partial})
	require.Equal(t, false, all["settlement_complete"])
	require.Nil(t, all["unresolved_native_calls"])
}

func TestJourneyStopInterruptedMockBillingStaysUnknown(t *testing.T) {
	full := mockBillingProof(2, 2, 2, 2, .2, nil)
	require.Equal(t, true, full["settlement_complete"])
	require.Equal(t, .2, full["full_local_usd"])
	partial := mockBillingProof(2, 1, 1, 1, .1, nil)
	require.Equal(t, false, partial["settlement_complete"])
	require.Nil(t, partial["full_local_usd"])
	require.Equal(t, 1, partial["unresolved_native_calls"])
	duplicate := mockBillingProof(2, 2, 1, 2, .2, nil)
	require.Equal(t, false, duplicate["settlement_complete"])
	require.Nil(t, duplicate["full_local_usd"])
	missing := mockBillingProof(1, 0, 0, 0, 0, fmt.Errorf("synthetic query failure"))
	require.False(t, missing["available"].(bool))
	require.Nil(t, missing["observed_rows"])
	require.Nil(t, missing["full_local_usd"])
}
