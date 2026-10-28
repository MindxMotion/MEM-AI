package middleware

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

func IntegrationTokenRateLimit() gin.HandlerFunc {
	return rateLimitFactory(20, 60, "integration-token")
}

func IntegrationBalanceAuth() gin.HandlerFunc {
	inMemoryRateLimiter.Init(common.RateLimitKeyExpirationDuration)
	return func(c *gin.Context) {
		parts := strings.Fields(c.GetHeader("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			c.Header("WWW-Authenticate", `Bearer realm="new-api-integration"`)
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
			return
		}
		client, err := service.ValidateIntegrationToken(c.Request.Context(), parts[1])
		if err != nil {
			if errors.Is(err, service.ErrIntegrationCredential) {
				c.Header("WWW-Authenticate", `Bearer realm="new-api-integration", error="invalid_token"`)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
				return
			}
			logger.LogError(c.Request.Context(), common.LogText("integration authentication unavailable (request_id=%s)", c.GetString(common.RequestIdKey)))
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "temporarily_unavailable"})
			return
		}
		key := redisRateLimitNamespace + ":integration:" + client.ClientID
		if common.RedisEnabled {
			allowed, _, ttl, err := redisFixedWindowTake(c.Request.Context(), key, 120, 60)
			if err != nil {
				logger.LogError(c.Request.Context(), common.LogText("integration rate limit unavailable (request_id=%s)", c.GetString(common.RequestIdKey)))
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "temporarily_unavailable"})
				return
			}
			if !allowed {
				writeRateLimited(c, ttl)
				return
			}
		} else if !inMemoryRateLimiter.Request(key, 120, 60) {
			writeRateLimited(c, 60)
			return
		}
		c.Set("integration_client", client)
		c.Next()
		if c.Writer.Status() < 500 {
			if err := model.TouchIntegrationClient(c.Request.Context(), client.Id, client.Version); err != nil {
				logger.LogError(c.Request.Context(), common.LogText("integration last-use update failed (client_id=%s request_id=%s)", client.ClientID, c.GetString(common.RequestIdKey)))
			}
		}
	}
}
