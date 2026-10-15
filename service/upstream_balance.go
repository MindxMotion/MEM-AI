package service

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/shopspring/decimal"
)

var balanceAlias = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var balanceUnit = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,15}$`)

type UpstreamBalanceSnapshot struct {
	Total         *float64 `json:"total"`
	Used          *float64 `json:"used"`
	Remaining     *float64 `json:"remaining"`
	Unit          string   `json:"unit"`
	Scope         string   `json:"scope"`
	Period        string   `json:"period"`
	Precision     int      `json:"precision"`
	CheckedAt     int64    `json:"checked_at"`
	Status        string   `json:"status"`
	Error         string   `json:"error,omitempty"`
	SharedAccount bool     `json:"shared_account"`
}

type balanceCacheEntry struct {
	snapshot UpstreamBalanceSnapshot
	attempt  time.Time
	pending  chan struct{}
}

// Identity contains a private credential digest and effective request semantics.
// It is never returned, logged or persisted. Cache eviction forgets old amounts.
type upstreamBalanceStore struct {
	mu      sync.Mutex
	origins map[string]string
	entries map[[32]byte]*balanceCacheEntry
	client  *http.Client
	slots   chan struct{}
	now     func() time.Time
	secret  string
}

var upstreamBalances = &upstreamBalanceStore{
	entries: make(map[[32]byte]*balanceCacheEntry), client: NewStrictProtectedFetchClient(),
	slots: make(chan struct{}, 4), now: time.Now,
	secret: rand.Text(),
}

func InitUpstreamBalanceOrigins() error {
	origins := map[string]string{}
	if raw := os.Getenv("UPSTREAM_BALANCE_ORIGINS"); raw != "" {
		if common.UnmarshalJsonStr(raw, &origins) != nil || origins == nil || len(origins) > 64 {
			return errors.New("invalid upstream balance origins configuration")
		}
	}
	for alias, origin := range origins {
		u, err := url.Parse(origin)
		if err != nil || !balanceAlias.MatchString(alias) || u.Scheme != "https" || u.Host == "" ||
			u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" ||
			u.RawPath != "" || u.ForceQuery {
			return errors.New("invalid upstream balance origins configuration")
		}
		p := &common.SSRFProtection{ApplyIPFilterForDomain: true}
		port := 443
		if u.Port() != "" {
			port, err = strconv.Atoi(u.Port())
			if err != nil {
				return errors.New("invalid upstream balance origins configuration")
			}
		}
		// Syntax and literal IP validation only at startup. Protected dialing
		// resolves every address under the request deadline before dispatch.
		if p.ValidateNetworkTarget(u.Hostname(), port) != nil {
			return errors.New("invalid upstream balance origins configuration")
		}
		u.Host = strings.ToLower(u.Host)
		if u.Port() == "443" {
			u.Host = strings.TrimSuffix(u.Host, ":443")
		}
		origins[alias] = u.String()
	}
	upstreamBalances.origins = origins
	return nil
}

func validBalancePath(path string) bool {
	if len(path) > 512 || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		strings.ContainsAny(path, `\%?#`) || strings.ContainsFunc(path, unicode.IsSpace) {
		return false
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	u, err := url.Parse(path)
	return err == nil && u.Host == "" && u.Scheme == "" && u.Path == path
}

func ValidateUpstreamBalanceProfile(p *dto.UpstreamBalanceProfile) error {
	if p == nil {
		return nil
	}
	if p.Profile != "legacy_subscription_usage" || !balanceAlias.MatchString(p.Origin) ||
		!balanceUnit.MatchString(p.Unit) || (p.Scope != "account" && p.Scope != "key") || p.Period != "cumulative" {
		return errors.New("Invalid upstream balance configuration")
	}
	if _, ok := upstreamBalances.origins[p.Origin]; !ok {
		return errors.New("Invalid upstream balance configuration")
	}
	for _, path := range []string{p.SubscriptionPath, p.UsagePath} {
		if path != "" && !validBalancePath(path) {
			return errors.New("Invalid upstream balance configuration")
		}
	}
	return nil
}

func ReadUpstreamBalanceSettings(raw string) (*dto.UpstreamBalanceProfile, error) {
	var settings dto.ChannelOtherSettings
	if strings.TrimSpace(raw) != "" && common.UnmarshalJsonStr(raw, &settings) != nil {
		return nil, errors.New("Invalid upstream balance configuration")
	}
	return settings.UpstreamBalance, nil
}

func (s *upstreamBalanceStore) identity(p dto.UpstreamBalanceProfile, key string) ([32]byte, dto.UpstreamBalanceProfile) {
	if p.SubscriptionPath == "" {
		p.SubscriptionPath = "/v1/dashboard/billing/subscription"
	}
	if p.UsagePath == "" {
		p.UsagePath = "/v1/dashboard/billing/usage"
	}
	effective := p
	effective.Origin = "" // Equivalent aliases for one exact origin share a snapshot.
	data, _ := common.Marshal(effective)
	fingerprint := hmac.New(sha256.New, []byte(s.secret))
	fingerprint.Write([]byte(s.origins[p.Origin] + "\x00" + key + "\x00" + string(data)))
	var identity [32]byte
	copy(identity[:], fingerprint.Sum(nil))
	return identity, p
}

func UpstreamBalance(ctx context.Context, p dto.UpstreamBalanceProfile, key string, refresh bool) UpstreamBalanceSnapshot {
	return upstreamBalances.get(ctx, p, key, refresh)
}

func (s *upstreamBalanceStore) get(ctx context.Context, p dto.UpstreamBalanceProfile, key string, refresh bool) UpstreamBalanceSnapshot {
	id, p := s.identity(p, key)
	unknown := UpstreamBalanceSnapshot{Unit: p.Unit, Scope: p.Scope, Period: p.Period, Precision: 2, Status: "unknown", SharedAccount: p.Scope == "account"}
	for {
		s.mu.Lock()
		entry := s.entries[id]
		if entry == nil {
			if !refresh {
				s.mu.Unlock()
				return unknown
			}
			if len(s.entries) >= 256 {
				var oldestID [32]byte
				var oldest *balanceCacheEntry
				for candidateID, candidate := range s.entries {
					if candidate.pending == nil && (oldest == nil || candidate.attempt.Before(oldest.attempt)) {
						oldestID, oldest = candidateID, candidate
					}
				}
				if oldest == nil {
					s.mu.Unlock()
					unknown.Error = "unavailable"
					unknown.Status = "error"
					return unknown
				}
				delete(s.entries, oldestID)
			}
			entry = &balanceCacheEntry{snapshot: unknown}
			s.entries[id] = entry
		}
		result := entry.snapshot
		if result.CheckedAt > 0 && s.now().Sub(time.Unix(result.CheckedAt, 0)) >= time.Minute && result.Status == "fresh" {
			result.Status = "stale"
		}
		if !refresh || (entry.pending == nil && (s.now().Sub(entry.attempt) < 10*time.Second ||
			(result.Status == "fresh" && s.now().Sub(time.Unix(result.CheckedAt, 0)) < time.Minute))) {
			s.mu.Unlock()
			return result
		}
		if pending := entry.pending; pending != nil {
			s.mu.Unlock()
			select {
			case <-pending:
				continue
			case <-ctx.Done():
				result.Error = "timeout"
				result.Status = "error"
				if result.CheckedAt > 0 {
					result.Status = "stale"
				}
				return result
			}
		}
		entry.pending = make(chan struct{})
		entry.attempt = s.now()
		s.mu.Unlock()
		fetchCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		var total, used float64
		category := ""
		select {
		case s.slots <- struct{}{}:
			total, used, category = s.fetch(fetchCtx, p, key)
			<-s.slots
		case <-fetchCtx.Done():
			category = "timeout"
		}
		cancel()
		s.mu.Lock()
		if category == "" {
			remaining, _ := decimal.NewFromFloat(total).Sub(decimal.NewFromFloat(used)).Float64()
			result = unknown
			result.Total = &total
			result.Used = &used
			result.Remaining = &remaining
			result.CheckedAt = s.now().Unix()
			result.Status = "fresh"
		} else {
			result = entry.snapshot
			result.Error = category
			result.Status = "error"
			if result.CheckedAt > 0 {
				result.Status = "stale"
			}
		}
		entry.snapshot = result
		close(entry.pending)
		entry.pending = nil
		s.mu.Unlock()
		return result
	}
}

func (s *upstreamBalanceStore) fetch(ctx context.Context, p dto.UpstreamBalanceProfile, key string) (float64, float64, string) {
	values := make([]float64, 2)
	for i, path := range []string{p.SubscriptionPath, p.UsagePath} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.origins[p.Origin]+path, nil)
		if err != nil {
			return 0, 0, "configuration"
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Accept", "application/json")
		resp, err := s.client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return 0, 0, "timeout"
			}
			return 0, 0, "unavailable"
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024+1))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return 0, 0, "authorization"
		}
		if resp.StatusCode != http.StatusOK {
			return 0, 0, "unavailable"
		}
		if err != nil {
			if ctx.Err() != nil {
				return 0, 0, "timeout"
			}
			return 0, 0, "invalid_response"
		}
		if len(body) > 256*1024 {
			return 0, 0, "invalid_response"
		}
		// Bound structural work before decoding; braces inside strings do not count.
		depth, inString, escaped := 0, false, false
		for _, char := range body {
			if inString {
				if escaped {
					escaped = false
					continue
				}
				if char == '\\' {
					escaped = true
					continue
				}
				if char == '"' {
					inString = false
				}
				continue
			}
			switch char {
			case '"':
				inString = true
			case '{', '[':
				depth++
				if depth > 64 {
					return 0, 0, "invalid_response"
				}
			case '}', ']':
				depth--
			}
		}
		var fields map[string]json.RawMessage
		if common.Unmarshal(body, &fields) != nil || fields["error"] != nil {
			return 0, 0, "invalid_response"
		}
		if rawSuccess, exists := fields["success"]; exists {
			var success bool
			if common.Unmarshal(rawSuccess, &success) != nil || !success {
				return 0, 0, "invalid_response"
			}
		}
		field := "hard_limit_usd"
		if i == 1 {
			field = "total_usage"
		}
		raw := strings.TrimSpace(string(fields[field]))
		if raw == "" || raw == "null" || raw[0] == '"' || len(raw) > 32 {
			return 0, 0, "invalid_response"
		}
		value, err := decimal.NewFromString(raw)
		if err != nil || value.Exponent() < -18 || value.Exponent() > 12 || value.IsNegative() || value.GreaterThan(decimal.NewFromInt(1_000_000_000_000)) {
			return 0, 0, "invalid_response"
		}
		if i == 1 {
			value = value.Div(decimal.NewFromInt(100))
		}
		values[i], _ = value.Float64()
	}
	return values[0], values[1], ""
}
