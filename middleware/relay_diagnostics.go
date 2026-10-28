package middleware

import (
	"strings"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
)

// RelayDiagnostics observes terminal HTTP results, including distribution and
// validation failures which never reach a provider. WebSockets and task polling
// have different lifecycles and are deliberately not HTTP request samples.
func RelayDiagnostics() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !constant.ErrorLogEnabled || c.Request.Method != "POST" {
			c.Next()
			return
		}
		endpoint := c.Request.URL.Path
		switch endpoint {
		case "/v1/chat/completions", "/v1/completions", "/v1/messages", "/v1/responses", "/v1/responses/compact",
			"/v1/embeddings", "/v1/rerank", "/v1/audio/transcriptions", "/v1/audio/translations", "/v1/audio/speech",
			"/pg/chat/completions":
		default:
			if strings.HasPrefix(endpoint, "/v1beta/models/") &&
				(strings.HasSuffix(endpoint, ":generateContent") || strings.HasSuffix(endpoint, ":streamGenerateContent")) {
				endpoint = "/v1beta/models/:model:generateContent"
			} else {
				c.Next()
				return
			}
		}
		started := time.Now()
		defer func() {
			recovered := recover()
			service.FinishRelayDiagnostic(c, endpoint, started, recovered != nil)
			if recovered != nil {
				panic(recovered)
			}
		}()
		c.Next()
	}
}
