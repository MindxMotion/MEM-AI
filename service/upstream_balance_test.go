package service

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpstreamBalanceValidation(t *testing.T) {
	t.Setenv("UPSTREAM_BALANCE_ORIGINS", `{"primary":"https://example.com"}`)
	oldOrigins := upstreamBalances.origins
	t.Cleanup(func() { upstreamBalances.origins = oldOrigins })
	require.NoError(t, InitUpstreamBalanceOrigins())
	p := dto.UpstreamBalanceProfile{Profile: "legacy_subscription_usage", Origin: "primary", Unit: "USD", Scope: "account", Period: "cumulative"}
	require.NoError(t, ValidateUpstreamBalanceProfile(&p))
	for _, path := range []string{"https://example.com/x", "//example.com/x", "/../x", "/a/./b", "/%2e%2e/x", "/x?key=x", "/x#x", `/\example.com`} {
		t.Run(path, func(t *testing.T) { p.SubscriptionPath = path; require.Error(t, ValidateUpstreamBalanceProfile(&p)) })
	}
	p.SubscriptionPath = "/billing/subscription"
	require.NoError(t, ValidateUpstreamBalanceProfile(&p))
	p.Period = "monthly"
	require.Error(t, ValidateUpstreamBalanceProfile(&p))
	for _, raw := range []string{`{"primary":"http://example.com"}`, `{"primary":"https://127.0.0.1"}`, `{"primary":"https://example.com/x"}`, `{"primary":"https://user:pass@example.com"}`, `not-json`} {
		t.Setenv("UPSTREAM_BALANCE_ORIGINS", raw)
		require.EqualError(t, InitUpstreamBalanceOrigins(), "invalid upstream balance origins configuration")
	}
}

func TestUpstreamBalanceNormalizationAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name, subscription, usage, category string
		status                              int
		total, used, remaining              float64
	}{
		{"observed shape", `{"hard_limit_usd":120,"object":"billing_subscription"}`, `{"total_usage":2500,"object":"list"}`, "", 200, 120, 25, 95},
		{"zero", `{"hard_limit_usd":0}`, `{"total_usage":0}`, "", 200, 0, 0, 0},
		{"overdrawn", `{"hard_limit_usd":100}`, `{"total_usage":12500}`, "", 200, 100, 125, -25},
		{"decimal", `{"hard_limit_usd":12.34}`, `{"total_usage":123}`, "", 200, 12.34, 1.23, 11.11},
		{"serialized floating usage", `{"hard_limit_usd":120}`, `{"total_usage":2500.12345678901234}`, "", 200, 120, 25.001234567890124, 94.99876543210988},
		{"fractional precision beyond bound", `{"hard_limit_usd":1e-19}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"missing", `{"credits":120}`, `{"total_usage":2500}`, "invalid_response", 200, 0, 0, 0},
		{"string number", `{"hard_limit_usd":"120"}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"error envelope", `{"hard_limit_usd":120,"error":{"message":"secret-body"}}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"failed envelope", `{"hard_limit_usd":120,"success":false}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"wrong success type", `{"hard_limit_usd":120,"success":"true"}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"wrong usage type", `{"hard_limit_usd":120}`, `{"total_usage":"2500"}`, "invalid_response", 200, 0, 0, 0},
		{"out of range", `{"hard_limit_usd":1000000000001}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"negative", `{"hard_limit_usd":-1}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"extreme exponent", `{"hard_limit_usd":1e-2147483647}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"malformed", `{`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"null", `{"hard_limit_usd":null}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"oversize", strings.Repeat(" ", 256*1024+1), `{}`, "invalid_response", 200, 0, 0, 0},
		{"depth", `{"extra":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `,"hard_limit_usd":120}`, `{}`, "invalid_response", 200, 0, 0, 0},
		{"redirect", `{"secret":"private-key"}`, `{}`, "unavailable", 302, 0, 0, 0},
		{"authorization", `{"secret":"private-key"}`, `{}`, "authorization", 401, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "Bearer fixture-key", r.Header.Get("Authorization"))
				if tc.status == http.StatusFound {
					w.Header().Set("Location", "/must-not-follow")
				}
				w.WriteHeader(tc.status)
				body := tc.subscription
				if r.URL.Path == "/alternate/usage" {
					body = tc.usage
				}
				_, err := io.WriteString(w, body)
				assert.NoError(t, err)
			}))
			defer server.Close()
			client := server.Client()
			client.CheckRedirect = NewStrictProtectedFetchClient().CheckRedirect
			store := upstreamBalanceStore{origins: map[string]string{"primary": server.URL}, client: client, now: time.Now, slots: make(chan struct{}, 4), entries: map[[32]byte]*balanceCacheEntry{}}
			p := dto.UpstreamBalanceProfile{Profile: "legacy_subscription_usage", Origin: "primary", Unit: "USD", Scope: "account", Period: "cumulative", SubscriptionPath: "/alternate/subscription", UsagePath: "/alternate/usage"}
			snapshot := store.get(context.Background(), p, "fixture-key", true)
			assert.Equal(t, tc.category, snapshot.Error)
			if tc.category == "" {
				require.NotNil(t, snapshot.Total)
				assert.Equal(t, tc.total, *snapshot.Total)
				assert.Equal(t, tc.used, *snapshot.Used)
				assert.Equal(t, tc.remaining, *snapshot.Remaining)
				assert.Equal(t, "fresh", snapshot.Status)
			} else {
				assert.Nil(t, snapshot.Total)
				assert.Equal(t, "error", snapshot.Status)
			}
			wire, err := common.Marshal(snapshot)
			require.NoError(t, err)
			assert.NotContains(t, string(wire), "fixture-key")
			assert.NotContains(t, string(wire), "secret-body")
			assert.NotContains(t, string(wire), server.URL)
			assert.NotContains(t, string(wire), "private-key")
			if tc.status == http.StatusFound {
				assert.EqualValues(t, 1, calls.Load(), "redirect target must never receive the credential")
			}
		})
	}

}

func TestUpstreamBalanceDecodedBodyAndCancellation(t *testing.T) {
	t.Run("compressed decoded body limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(w)
			_, err := io.WriteString(writer, strings.Repeat(" ", 256*1024)+`{"hard_limit_usd":120}`)
			assert.NoError(t, err)
			assert.NoError(t, writer.Close())
		}))
		defer server.Close()
		store := upstreamBalanceStore{origins: map[string]string{"primary": server.URL}, client: server.Client()}
		_, _, category := store.fetch(context.Background(), dto.UpstreamBalanceProfile{Origin: "primary", SubscriptionPath: "/subscription"}, "fixture-key")
		assert.Equal(t, "invalid_response", category)
	})
	t.Run("cancellation while reading body", func(t *testing.T) {
		arrived := make(chan struct{})
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(arrived)
			<-r.Context().Done()
		}))
		defer server.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := upstreamBalanceStore{origins: map[string]string{"primary": server.URL}, client: server.Client()}
		result := make(chan string, 1)
		go func() {
			_, _, category := store.fetch(ctx, dto.UpstreamBalanceProfile{Origin: "primary", SubscriptionPath: "/subscription"}, "fixture-key")
			result <- category
		}()
		<-arrived
		cancel()
		assert.Equal(t, "timeout", <-result)
	})
}

func TestUpstreamBalanceSharedCacheRetainsLastKnown(t *testing.T) {
	var calls atomic.Int32
	var failing atomic.Bool
	arrived, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(arrived)
			<-release
		}
		if failing.Load() {
			w.WriteHeader(502)
			return
		}
		body := `{"hard_limit_usd":120}`
		if strings.HasSuffix(r.URL.Path, "usage") {
			body = `{"total_usage":2500}`
		}
		_, err := io.WriteString(w, body)
		assert.NoError(t, err)
	}))
	defer server.Close()
	now := time.Unix(1000, 0)
	store := upstreamBalanceStore{origins: map[string]string{"primary": server.URL, "secondary": server.URL}, client: server.Client(), now: func() time.Time { return now }, slots: make(chan struct{}, 4), entries: map[[32]byte]*balanceCacheEntry{}}
	p := dto.UpstreamBalanceProfile{Profile: "legacy_subscription_usage", Origin: "primary", Unit: "USD", Scope: "account", Period: "cumulative"}
	assert.Equal(t, "unknown", store.get(context.Background(), p, "fixture-key", false).Status)
	assert.Zero(t, calls.Load())
	var group sync.WaitGroup
	results := make(chan UpstreamBalanceSnapshot, 2)
	group.Go(func() { results <- store.get(context.Background(), p, "fixture-key", true) })
	<-arrived
	group.Go(func() { results <- store.get(context.Background(), p, "fixture-key", true) })
	close(release)
	group.Wait()
	first := <-results
	second := <-results
	assert.Equal(t, first, second)
	assert.EqualValues(t, 2, calls.Load())
	assert.Equal(t, first, store.get(context.Background(), p, "fixture-key", true))
	alias := p
	alias.Origin = "secondary"
	assert.Equal(t, first, store.get(context.Background(), alias, "fixture-key", true))
	changedUnit := p
	changedUnit.Unit = "credits"
	assert.Nil(t, store.get(context.Background(), changedUnit, "fixture-key", false).Total)
	now = now.Add(61 * time.Second)
	failing.Store(true)
	stale := store.get(context.Background(), p, "fixture-key", true)
	require.NotNil(t, stale.Remaining)
	assert.Equal(t, 95.0, *stale.Remaining)
	assert.Equal(t, first.CheckedAt, stale.CheckedAt)
	assert.Equal(t, "stale", stale.Status)
	assert.Equal(t, "unavailable", stale.Error)
	assert.Nil(t, store.get(context.Background(), p, "different-fixture-key", false).Total)
}

func TestUpstreamBalanceStrictTransport(t *testing.T) {
	client := NewStrictProtectedFetchClient()
	transport := client.Transport.(*http.Transport)
	assert.Nil(t, transport.Proxy)
	assert.True(t, transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify)
	assert.ErrorIs(t, client.CheckRedirect(nil, nil), http.ErrUseLastResponse)
	for _, address := range []string{"127.0.0.1:80", "[::1]:443", "169.254.169.254:80"} {
		_, err := transport.DialContext(context.Background(), "tcp", address)
		require.Error(t, err)
	}
	var dialed bool
	dialer := protectedFetchDialer{
		resolver:      staticSSRFResolver{"example.com": {{IP: net.ParseIP("8.8.8.8")}, {IP: net.ParseIP("127.0.0.1")}}},
		dialContext:   func(context.Context, string, string) (net.Conn, error) { dialed = true; return nil, nil },
		getProtection: staticProtection(&common.SSRFProtection{ApplyIPFilterForDomain: true}),
	}
	_, err := dialer.DialContext(context.Background(), "tcp", "example.com:443")
	require.Error(t, err)
	assert.False(t, dialed)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := dto.UpstreamBalanceProfile{Origin: "primary", SubscriptionPath: "/subscription", UsagePath: "/usage"}
	store := upstreamBalanceStore{origins: map[string]string{"primary": "https://example.com"}, client: client}
	_, _, category := store.fetch(ctx, p, "fixture-key")
	assert.Equal(t, "timeout", category)

	// The dial address is the validated IP, not a second hostname lookup.
	var address string
	dialer.resolver = staticSSRFResolver{"example.com": {{IP: net.ParseIP("8.8.8.8")}}}
	dialer.dialContext = func(_ context.Context, _ string, target string) (net.Conn, error) {
		address = target
		return nil, nil
	}
	_, err = dialer.DialContext(context.Background(), "tcp", "example.com:443")
	require.NoError(t, err)
	assert.Equal(t, "8.8.8.8:443", address)
}

func TestUpstreamBalanceRejectsWhitespacePaths(t *testing.T) {
	for _, path := range []string{"/billing subscription", "/billing\tusage", "/billing\nusage"} {
		assert.False(t, validBalancePath(path))
	}
}

func TestUpstreamBalanceCacheEvictsOldestAccount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := `{"hard_limit_usd":120}`
		if strings.HasSuffix(r.URL.Path, "usage") {
			body = `{"total_usage":2500}`
		}
		_, err := io.WriteString(w, body)
		assert.NoError(t, err)
	}))
	defer server.Close()
	now := time.Unix(1000, 0)
	store := upstreamBalanceStore{origins: map[string]string{"primary": server.URL}, client: server.Client(),
		now: func() time.Time { return now }, slots: make(chan struct{}, 4), entries: map[[32]byte]*balanceCacheEntry{}}
	p := dto.UpstreamBalanceProfile{Profile: "legacy_subscription_usage", Origin: "primary", Unit: "USD", Scope: "account", Period: "cumulative"}
	// One past the documented capacity is a boundary fixture, not a stress test.
	for i := range 257 {
		snapshot := store.get(context.Background(), p, fmt.Sprintf("fixture-%d", i), true)
		require.Equal(t, "fresh", snapshot.Status)
		now = now.Add(time.Second)
	}
	evicted := store.get(context.Background(), p, "fixture-0", false)
	assert.Equal(t, "unknown", evicted.Status)
	assert.Nil(t, evicted.Remaining)
	retained := store.get(context.Background(), p, "fixture-256", false)
	require.NotNil(t, retained.Remaining)
	assert.Equal(t, 95.0, *retained.Remaining)
}
