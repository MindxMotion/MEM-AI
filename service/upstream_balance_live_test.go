package service

import (
	"context"
	"math"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/require"
)

// Opt-in, read-only compatibility gate. The origin and existing credential are
// supplied by a protected operator process, never fixtures or public source.
func TestUpstreamBalanceLiveCompatibility(t *testing.T) {
	if os.Getenv("BALANCE_LIVE_READ_ONLY") != "1" {
		t.Skip("protected read-only compatibility gate not enabled")
	}
	key := os.Getenv("BALANCE_LIVE_KEY")
	require.True(t, key != "", "protected existing credential required")
	previousOrigins := upstreamBalances.origins
	t.Cleanup(func() { upstreamBalances.origins = previousOrigins })
	require.NoError(t, InitUpstreamBalanceOrigins())
	profile := dto.UpstreamBalanceProfile{Profile: "legacy_subscription_usage",
		Origin: "primary", Unit: "USD", Scope: "account", Period: "cumulative"}
	require.NoError(t, ValidateUpstreamBalanceProfile(&profile))
	snapshot := UpstreamBalance(context.Background(), profile, key, true)
	require.Equal(t, "", snapshot.Error)
	require.Equal(t, "fresh", snapshot.Status)
	require.True(t, snapshot.Total != nil && snapshot.Used != nil && snapshot.Remaining != nil,
		"complete normalized values required")
	require.True(t, !math.IsNaN(*snapshot.Total) && !math.IsInf(*snapshot.Total, 0) &&
		!math.IsNaN(*snapshot.Used) && !math.IsInf(*snapshot.Used, 0) &&
		!math.IsNaN(*snapshot.Remaining) && !math.IsInf(*snapshot.Remaining, 0), "finite values required")
	require.True(t, *snapshot.Total >= 0 && *snapshot.Used >= 0, "nonnegative total and used required")
	require.True(t, math.Abs(*snapshot.Total-*snapshot.Used-*snapshot.Remaining) <=
		math.Max(0.000001, *snapshot.Total*1e-12), "remaining arithmetic must agree")
	cached := UpstreamBalance(context.Background(), profile, key, true)
	require.True(t, cached.CheckedAt == snapshot.CheckedAt && cached.Total != nil &&
		cached.Used != nil && cached.Remaining != nil && *cached.Total == *snapshot.Total &&
		*cached.Used == *snapshot.Used && *cached.Remaining == *snapshot.Remaining,
		"cooldown returns identical snapshot without another fetch")
	t.Log("protected TLS compatibility: complete finite pair, arithmetic and cooldown passed; values withheld")
}
