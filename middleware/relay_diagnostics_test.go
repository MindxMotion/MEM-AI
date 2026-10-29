package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	hostdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestRelayDiagnosticsTerminalOutcomes(t *testing.T) {
	previousDB, previousEnabled := model.DB, constant.ErrorLogEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.RelayOutcome{}))
	model.DB, constant.ErrorLogEnabled = db, true
	model.StartRelayOutcomeWriter()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, model.StopRelayOutcomeWriter(ctx))
		model.DB, constant.ErrorLogEnabled = previousDB, previousEnabled
		require.NoError(t, sqlDB.Close())
	})
	for _, test := range []struct {
		name        string
		path        string
		wantOutcome string
		wantReason  string
		handler     gin.HandlerFunc
	}{
		{"success", "/v1/chat/completions", "success", "completed", func(c *gin.Context) { c.Status(200) }},
		{"validation", "/v1/chat/completions", "rejected", "request_rejected", func(c *gin.Context) { c.AbortWithStatus(400) }},
		{"unauthenticated", "/v1/chat/completions", "rejected", "request_rejected", func(c *gin.Context) { c.Set("id", 0); c.AbortWithStatus(401) }},
		{"unauthenticated-existing-owner", "/v1/chat/completions", "rejected", "request_rejected", func(c *gin.Context) {
			c.Set("username", "")
			c.AbortWithStatus(401)
		}},
		{"unavailable", "/v1/embeddings", "failure", "channel_unavailable", func(c *gin.Context) {
			service.SetRelayDiagnosticResult(c, nil, types.NewErrorWithStatusCode(errors.New("private upstream error"), types.ErrorCodeModelNotFound, 503))
			c.AbortWithStatus(503)
		}},
		{"truncated-stream", "/v1/chat/completions", "failure", "missing_terminal_event", func(c *gin.Context) {
			stream := relaycommon.NewStreamStatus()
			stream.RequireTerminal()
			stream.SetEndReason(relaycommon.StreamEndReasonEOF, nil)
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, stream)
			c.Header("Content-Type", "text/event-stream")
			_, err := c.Writer.Write([]byte("data: partial\n\n"))
			require.NoError(t, err)
		}},
		{"failed-stream", "/v1/messages", "failure", "upstream_stream_failed", func(c *gin.Context) {
			stream := relaycommon.NewStreamStatus()
			stream.MarkFailed("provider_code", "provider_type", 500)
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, stream)
			c.Status(200)
		}},
		{"cancelled", "/v1/messages", "cancelled", "client_disconnected", func(c *gin.Context) {
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}},
		{"completed-before-cancel", "/v1/messages", "success", "completed", func(c *gin.Context) {
			stream := relaycommon.NewStreamStatus()
			stream.MarkCompleted()
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, stream)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}},
		{"nonstream-completed-before-cancel", "/v1/messages", "success", "completed", func(c *gin.Context) {
			service.SetRelayDiagnosticResult(c, &relaycommon.RelayInfo{IsStream: false}, nil)
			_, err := c.Writer.Write([]byte(`{"content":[{"type":"text","text":"done"}]}`))
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}},
		{"nonstream-cancel-before-response", "/v1/messages", "cancelled", "client_disconnected", func(c *gin.Context) {
			service.SetRelayDiagnosticResult(c, &relaycommon.RelayInfo{IsStream: false}, nil)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}},
		{"nonstream-failed-before-cancel", "/v1/messages", "cancelled", "client_disconnected", func(c *gin.Context) {
			service.SetRelayDiagnosticResult(c, &relaycommon.RelayInfo{IsStream: false},
				types.NewErrorWithStatusCode(errors.New("upstream failure"), types.ErrorCodeDoRequestFailed, 502))
			c.AbortWithStatus(502)
			ctx, cancel := context.WithCancel(c.Request.Context())
			cancel()
			c.Request = c.Request.WithContext(ctx)
		}},
		{"timeout", "/v1/messages", "failure", "timeout", func(c *gin.Context) {
			stream := relaycommon.NewStreamStatus()
			stream.SetEndReason(relaycommon.StreamEndReasonTimeout, nil)
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, stream)
		}},
		{"deadline-not-cancel", "/v1/messages", "failure", "timeout", func(c *gin.Context) {
			ctx, cancel := context.WithDeadline(c.Request.Context(), time.Unix(1, 0))
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			stream := relaycommon.NewStreamStatus()
			stream.SetEndReason(relaycommon.StreamEndReasonClientGone, context.DeadlineExceeded)
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, stream)
		}},
		{"incomplete", "/v1/messages", "incomplete", "upstream_incomplete", func(c *gin.Context) {
			stream := relaycommon.NewStreamStatus()
			stream.MarkIncomplete("max_output_tokens")
			common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, stream)
		}},
		{"panic", "/v1/chat/completions", "failure", "panic", func(c *gin.Context) { panic("fixture panic") }},
		{"playground", "/pg/chat/completions", "success", "completed", func(c *gin.Context) { c.Status(200) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			router := gin.New()
			router.Use(gin.CustomRecovery(func(c *gin.Context, recovered any) { c.AbortWithStatus(500) }))
			router.Use(RequestId(), RelayDiagnostics())
			router.POST(test.path, func(c *gin.Context) {
				c.Set("id", 7)
				c.Set("username", "private-user")
				c.Set("original_model", "test-model")
				c.Set("channel_id", 14)
				service.RequestPolicy(c).BeginAttempt(&model.Channel{Id: 13}, "default")
				service.RequestPolicy(c).BeginAttempt(&model.Channel{Id: 14}, "default")
				service.CaptureRelayRequestFeatures(c, []byte(`{"stream":true,"tools":[{"name":"private-tool"}],"response_format":{"type":"json_schema"},"thinking":{"type":"enabled"},"messages":[{"role":"user","content":[{"type":"image","source":{"data":"private-image"}}]}]}`))
			}, test.handler)
			request := httptest.NewRequest(http.MethodPost, test.path, strings.NewReader(`{"private":"prompt"}`))
			request.Header.Set("User-Agent", "WorkBuddy/private-version")
			request.Header.Set("Authorization", "Bearer private-credential")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			// Drain all accepted records without assuming a timing-dependent write.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, model.StopRelayOutcomeWriter(ctx))
			var outcome model.RelayOutcome
			require.NoError(t, db.Where("request_id = ?", recorder.Header().Get(common.RequestIdKey)).First(&outcome).Error)
			assert.Equal(t, test.wantOutcome, outcome.Outcome)
			assert.Equal(t, test.wantReason, outcome.Reason)
			assert.Equal(t, "workbuddy", outcome.ClientFamily)
			assert.Equal(t, test.path, outcome.Endpoint)
			assert.Equal(t, 2, outcome.Attempts)
			assert.Equal(t, 14, outcome.ChannelID)
			assert.Equal(t, !strings.HasPrefix(test.name, "unauthenticated"), outcome.Authenticated)
			assert.True(t, outcome.FeaturesKnown)
			assert.True(t, outcome.HasTools)
			assert.True(t, outcome.HasImages)
			assert.True(t, outcome.Structured)
			assert.True(t, outcome.Reasoning)
			assert.Equal(t, "lt_4k", outcome.RequestSize)
			assert.EqualValues(t, -1, outcome.FirstResponseMS)
			if test.name == "panic" {
				assert.Equal(t, 500, outcome.HTTPStatus)
			}
			if test.name == "failed-stream" {
				assert.Equal(t, 500, outcome.ErrorStatus)
			}
			if test.name == "playground" {
				assert.Equal(t, "playground", outcome.Origin)
			}
			encoded, err := common.Marshal(outcome)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "private")
			var count int64
			require.NoError(t, db.Model(&model.RelayOutcome{}).Where("request_id = ?", outcome.RequestID).Count(&count).Error)
			assert.EqualValues(t, 1, count)
			model.StartRelayOutcomeWriter()
		})
	}
}

func TestRelayDiagnosticsClientFamilies(t *testing.T) {
	for agent, expected := range map[string]string{
		"WorkBuddy/1": "workbuddy", "claude-cli/1": "claude-code",
		"openai-python/1": "openai-sdk", "Anthropic/Python": "anthropic-sdk",
		"curl/8": "curl", "Mozilla/5": "browser", "private-unknown-value": "unknown",
	} {
		assert.Equal(t, expected, service.RelayClientFamily(agent))
	}
}

func TestRelayDistributionRejectsIncapablePinnedChannelBeforeUpstream(t *testing.T) {
	previousDB, previousCache, previousRedis := model.DB, common.MemoryCacheEnabled, common.RedisEnabled
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	model.DB, common.MemoryCacheEnabled, common.RedisEnabled = db, false, false
	t.Cleanup(func() {
		model.DB, common.MemoryCacheEnabled, common.RedisEnabled = previousDB, previousCache, previousRedis
		require.NoError(t, sqlDB.Close())
	})
	require.NoError(t, i18n.Init())
	channel := &model.Channel{Type: constant.ChannelTypeAnthropic, Status: common.ChannelStatusEnabled, Key: "never-sent"}
	require.NoError(t, db.Create(channel).Error)
	router := gin.New()
	reachedUpstream := false
	router.POST("/v1/embeddings", func(c *gin.Context) {
		c.Set("id", 7)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		service.GetChannelConstraints(c).AddPin(hostdto.ChannelPin{
			ChannelId: channel.Id, Source: hostdto.PinSourceToken,
			Rank: hostdto.PinRankToken, RetryMode: hostdto.PinRetrySingleAttempt,
		})
	}, Distribute(), func(c *gin.Context) { reachedUpstream = true })
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"text-embedding-3-small","input":"private prompt"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.False(t, reachedUpstream)
	assert.NotContains(t, recorder.Body.String(), "not implemented")
}
