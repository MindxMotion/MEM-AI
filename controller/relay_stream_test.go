package controller

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relay/channel"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRelayKeepAliveDuringImagePreparation(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	previousCount, previousMedia := constant.CountToken, constant.GetMediaToken
	fetchSettings := system_setting.GetFetchSetting()
	previousFetchSettings := *fetchSettings
	fetchSettings.AllowPrivateIp = true
	fetchSettings.AllowedPorts = []string{"1-65535"}
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 1
	constant.CountToken, constant.GetMediaToken = true, true
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = previousEnabled, previousInterval
		constant.CountToken, constant.GetMediaToken = previousCount, previousMedia
		*fetchSettings = previousFetchSettings
	})
	service.InitHttpClient()

	imageRequested := make(chan struct{})
	releaseImage := make(chan struct{})
	var releaseOnce sync.Once
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(imageRequested)
		<-releaseImage
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer imageServer.Close()
	engine := gin.New()
	engine.POST("/v1/messages", func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyOriginalModel, "claude-sonnet-4-20250514")
		Relay(c, types.RelayFormatClaude)
	})
	gateway := httptest.NewServer(engine)
	defer gateway.Close()
	defer releaseOnce.Do(func() { close(releaseImage) })

	body := `{"model":"claude-sonnet-4-20250514","max_tokens":16,"stream":true,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"` + imageServer.URL + `"}}]}]}`
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Post(gateway.URL+"/v1/messages", "application/json", strings.NewReader(body))
	require.NoError(t, err, "the gateway must send keepalive while its image download is blocked")
	defer response.Body.Close()
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "text/event-stream", response.Header.Get("Content-Type"))
	reader := bufio.NewReader(response.Body)
	for range 2 {
		line, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, ": PING\n", line)
		line, readErr = reader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, "\n", line)
	}
	select {
	case <-imageRequested:
	default:
		t.Fatal("the request never reached image preparation")
	}
	releaseOnce.Do(func() { close(releaseImage) })
	remaining, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(remaining), "event: error\n")
	require.Contains(t, string(remaining), `"type":"error"`)
	require.Contains(t, string(remaining), "data: ")
}

func TestRelayKeepAliveAcrossUpstreamRetries(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 1
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = previousEnabled, previousInterval
	})
	service.InitHttpClient()
	var attempts atomic.Int32
	retryStarted, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		close(retryStarted)
		select {
		case <-release:
			_, _ = io.WriteString(w, "data: ok\n\n")
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	engine := gin.New()
	engine.POST("/v1/messages", func(c *gin.Context) {
		stop := helper.StartStreamKeepAlive(c, 10*time.Millisecond)
		defer stop()
		if !assert.NoError(t, http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(5*time.Second))) {
			return
		}
		info := &relaycommon.RelayInfo{IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{}}
		for range 2 {
			request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
			if !assert.NoError(t, err) {
				return
			}
			response, err := channel.DoRequest(c, request, info)
			if !assert.NoError(t, err) {
				return
			}
			if response.StatusCode == http.StatusTooManyRequests {
				response.Body.Close()
				continue
			}
			_, _ = io.Copy(c.Writer, response.Body)
			response.Body.Close()
		}
	})
	gateway := httptest.NewServer(engine)
	defer gateway.Close()
	defer releaseOnce.Do(func() { close(release) })
	client := &http.Client{Timeout: 4 * time.Second}
	response, err := client.Post(gateway.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	defer response.Body.Close()
	select {
	case <-retryStarted:
	case <-time.After(time.Second):
		t.Fatal("the upstream retry did not start")
	}
	reader := bufio.NewReader(response.Body)
	for range 2 {
		line, readErr := reader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, ": PING\n", line)
		line, readErr = reader.ReadString('\n')
		require.NoError(t, readErr)
		require.Equal(t, "\n", line)
	}
	releaseOnce.Do(func() { close(release) })
	body, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(body), "data: ok\n\n")
	require.Equal(t, int32(2), attempts.Load())
}

func TestRelayKeepAlivePreservesValidationErrors(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled := settings.PingIntervalEnabled
	settings.PingIntervalEnabled = true
	t.Cleanup(func() { settings.PingIntervalEnabled = previousEnabled })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-20250514","stream":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	Relay(c, types.RelayFormatClaude)
	require.Equal(t, http.StatusBadRequest, recorder.Code)
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	require.NotContains(t, recorder.Body.String(), ": PING")
}

func TestStreamKeepAlivePreservesFastHTTPFailures(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval := settings.PingIntervalEnabled, settings.PingIntervalSeconds
	settings.PingIntervalEnabled, settings.PingIntervalSeconds = true, 60
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds = previousEnabled, previousInterval
	})
	service.InitHttpClient()
	for _, status := range []int{http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusTooManyRequests} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":"request rejected"}`)
			}))
			defer upstream.Close()
			engine := gin.New()
			engine.POST("/v1/messages", func(c *gin.Context) {
				stop := helper.StartStreamKeepAlive(c, time.Minute)
				defer stop()
				request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, upstream.URL, strings.NewReader("{}"))
				if !assert.NoError(t, err) {
					return
				}
				info := &relaycommon.RelayInfo{IsStream: true, ChannelMeta: &relaycommon.ChannelMeta{}}
				response, err := channel.DoRequest(c, request, info)
				if !assert.NoError(t, err) {
					return
				}
				defer response.Body.Close()
				body, err := io.ReadAll(response.Body)
				if assert.NoError(t, err) {
					c.Header("Content-Type", "application/json")
					c.Data(response.StatusCode, "application/json", body)
				}
			})
			gateway := httptest.NewServer(engine)
			defer gateway.Close()
			client := &http.Client{Timeout: 4 * time.Second}
			response, err := client.Post(gateway.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, status, response.StatusCode)
			require.Equal(t, "application/json", response.Header.Get("Content-Type"))
			require.JSONEq(t, `{"error":"request rejected"}`, string(body))
		})
	}
}

func TestRelayKeepAliveHonorsStreamSettings(t *testing.T) {
	settings := operation_setting.GetGeneralSetting()
	previousEnabled := settings.PingIntervalEnabled
	previousCount, previousMedia, previousNonStreamMedia := constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream
	constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = true, true, true
	t.Cleanup(func() {
		settings.PingIntervalEnabled = previousEnabled
		constant.CountToken, constant.GetMediaToken, constant.GetMediaTokenNotStream = previousCount, previousMedia, previousNonStreamMedia
	})
	for _, test := range []struct {
		name    string
		enabled bool
		stream  bool
	}{
		{name: "ping disabled", enabled: false, stream: true},
		{name: "non-streaming", enabled: true, stream: false},
		{name: "fast streaming failure", enabled: true, stream: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings.PingIntervalEnabled = test.enabled
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			body := `{"model":"claude-sonnet-4-20250514","max_tokens":16,"stream":` + strconv.FormatBool(test.stream) + `,"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"!"}}]}]}`
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			c.Request.Header.Set("Content-Type", "application/json")
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "claude-sonnet-4-20250514")
			Relay(c, types.RelayFormatClaude)
			require.Equal(t, http.StatusInternalServerError, recorder.Code)
			require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
			require.NotContains(t, recorder.Body.String(), ": PING")
			var response map[string]any
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			require.Equal(t, "error", response["type"])
		})
	}
}

func TestRelayStreamErrorAfterKeepAlive(t *testing.T) {
	for _, format := range []types.RelayFormat{types.RelayFormatClaude, types.RelayFormatOpenAI} {
		t.Run(string(format), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/stream", nil)
			helper.SetEventStreamHeaders(c)
			require.NoError(t, helper.PingData(c))
			common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.example")
			apiError := types.NewErrorWithStatusCode(errors.New("upstream rejected input at https://provider.example/v1/messages"), types.ErrorCodeBadResponseStatusCode, http.StatusBadRequest)
			writeRelayStreamError(c, format, apiError)
			require.Equal(t, http.StatusOK, recorder.Code)
			require.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
			require.Contains(t, recorder.Body.String(), "upstream rejected input")
			require.NotContains(t, recorder.Body.String(), "provider.example")
			status, ok := common.GetContextKeyType[*relaycommon.StreamStatus](c, constant.ContextKeyResponseStreamStatus)
			require.True(t, ok)
			require.True(t, status.ResponseFailed(), "stream errors must not consume the successful-request rate limit")
			require.Equal(t, http.StatusBadRequest, status.OutcomeSnapshot().ErrorStatus)
			if format == types.RelayFormatClaude {
				require.Contains(t, recorder.Body.String(), "event: error\n")
			} else {
				require.Contains(t, recorder.Body.String(), "data: [DONE]")
			}
		})
	}
}

func TestStreamKeepAliveCancellation(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	requestContext, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/stream", nil).WithContext(requestContext)
	stop := helper.StartStreamKeepAlive(c, time.Hour)
	cancel()
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("keepalive did not stop after client cancellation")
	}
	_, err := c.Writer.WriteString("data: done\n\n")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(recorder.Body.String(), "data: done\n\n"))
}

type heldClaudeStreamBody struct {
	io.Reader
	closed chan struct{}
	once   sync.Once
}

func (body *heldClaudeStreamBody) Read(data []byte) (int, error) {
	n, err := body.Reader.Read(data)
	if err == io.EOF {
		<-body.closed
	}
	return n, err
}

func (body *heldClaudeStreamBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

func TestClaudeStreamCompletesWithoutUpstreamEOF(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := &heldClaudeStreamBody{
		Reader: strings.NewReader(
			"data: " + `{"type":"message_start","message":{"id":"msg_test","model":"claude-test","usage":{"input_tokens":2}}}` + "\n\n" +
				"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
				"data: " + `{"type":"message_stop"}` + "\n\n"),
		closed: make(chan struct{}),
	}
	defer body.Close()
	info := &relaycommon.RelayInfo{
		RelayFormat: types.RelayFormatClaude, IsStream: true, DisablePing: true, StartTime: time.Now(),
		ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
	}
	var usage *dto.Usage
	var apiError *types.NewAPIError
	finished := make(chan struct{})
	go func() {
		usage, apiError = claude.ClaudeStreamHandler(c, &http.Response{Header: make(http.Header), Body: body}, info)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		require.NoError(t, body.Close())
		<-finished
		t.Fatal("message_stop must finish the response without waiting for upstream EOF")
	}
	require.Nil(t, apiError)
	require.NotNil(t, usage)
	assert.Equal(t, 2, usage.PromptTokens)
	assert.Equal(t, 3, usage.CompletionTokens)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
	assert.Nil(t, relayStreamFailure(c, info))
	assert.Equal(t, 1, strings.Count(recorder.Body.String(), `"type":"message_stop"`))
	select {
	case <-body.closed:
	default:
		t.Fatal("the completed upstream response body must be closed")
	}
}

type failedClaudeStreamWriter struct {
	gin.ResponseWriter
	writeError   error
	flushError   error
	terminalOnly bool
	terminal     bool
}

func (writer *failedClaudeStreamWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "event: message_stop") {
		writer.terminal = true
	}
	if writer.writeError != nil && (!writer.terminalOnly || writer.terminal) {
		return 0, writer.writeError
	}
	return writer.ResponseWriter.Write(data)
}

func (writer *failedClaudeStreamWriter) FlushError() error {
	if writer.terminalOnly && !writer.terminal {
		return nil
	}
	return writer.flushError
}

func TestClaudeStreamCompletionRequiresSuccessfulDelivery(t *testing.T) {
	for _, test := range []struct {
		name       string
		writeError error
		flushError error
		cancelled  bool
	}{
		{name: "write failed", writeError: io.ErrClosedPipe},
		{name: "flush failed", flushError: io.ErrClosedPipe},
		{name: "client cancelled", cancelled: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
			c.Writer = &failedClaudeStreamWriter{ResponseWriter: c.Writer, writeError: test.writeError, flushError: test.flushError}
			if test.cancelled {
				cancel()
			}
			info := &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatClaude, ChannelMeta: &relaycommon.ChannelMeta{},
				StreamStatus: relaycommon.NewStreamStatus(),
			}
			apiError := claude.HandleStreamResponseData(c, info, &claude.ClaudeResponseInfo{Usage: &dto.Usage{}}, `{"type":"message_stop"}`)
			require.NotNil(t, apiError)
			assert.True(t, types.IsSkipRetryError(apiError), "a failed downstream write must never replay upstream generation")
			assert.NotEqual(t, "completed", info.StreamStatus.ResponseOutcome())
			assert.Equal(t, "stop", service.DecideRelayRetry(c, apiError, 1).Action)
			err := helper.ClaudeData(c, dto.ClaudeResponse{Type: "message_stop"})
			assert.Error(t, err, "converted Claude events must also surface delivery errors")

			if test.cancelled {
				return
			}
			c, _ = gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			c.Writer = &failedClaudeStreamWriter{
				ResponseWriter: c.Writer, writeError: test.writeError, flushError: test.flushError, terminalOnly: true,
			}
			info = &relaycommon.RelayInfo{
				RelayFormat: types.RelayFormatClaude, IsStream: true, DisablePing: true, StartTime: time.Now(),
				ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "claude-test"},
			}
			previousTimeout := constant.StreamingTimeout
			constant.StreamingTimeout = 30
			defer func() { constant.StreamingTimeout = previousTimeout }()
			body := "data: " + `{"type":"message_start","message":{"id":"msg_test","model":"claude-test","usage":{"input_tokens":2}}}` + "\n\n" +
				"data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}` + "\n\n" +
				"data: " + `{"type":"message_stop"}` + "\n\n"
			usage, apiError := claude.ClaudeStreamHandler(c, &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, info)
			require.Nil(t, apiError, "delivery failure must retain partial usage for the existing settlement path")
			require.NotNil(t, usage)
			assert.Equal(t, 2, usage.PromptTokens)
			assert.Equal(t, 3, usage.CompletionTokens)
			assert.True(t, info.StreamStatus.HasErrors())
			assert.NotEqual(t, "completed", info.StreamStatus.ResponseOutcome())
			service.MarkRequestPolicySuccess(c, info.StreamStatus)
			assert.False(t, service.RequestPolicy(c).Successful)
		})
	}
}

func TestRelayStreamFailureNotification(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, test := range []struct {
		name      string
		end       relaycommon.StreamEndReason
		terminal  string
		status    int
		message   string
		cancelled bool
		nonStream bool
		legacyEOF bool
	}{
		{name: "idle timeout", end: relaycommon.StreamEndReasonTimeout, status: 504, message: "The upstream response stream timed out before completion"},
		{name: "read interrupted", end: relaycommon.StreamEndReasonScannerErr, status: 502, message: "The upstream response stream was interrupted"},
		{name: "missing terminal", end: relaycommon.StreamEndReasonEOF, status: 502, message: "The upstream response stream ended without a completion event"},
		{name: "completed", end: relaycommon.StreamEndReasonDone, terminal: "completed"},
		{name: "upstream error already delivered", end: relaycommon.StreamEndReasonEOF, terminal: "failed"},
		{name: "incomplete terminal already delivered", end: relaycommon.StreamEndReasonEOF, terminal: "incomplete"},
		{name: "client cancelled", end: relaycommon.StreamEndReasonClientGone, cancelled: true},
		{name: "non stream", end: relaycommon.StreamEndReasonEOF, nonStream: true},
		{name: "legacy protocol without terminal contract", end: relaycommon.StreamEndReasonEOF, legacyEOF: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
			c.Request.Header.Set("Accept-Language", "en")
			require.NoError(t, helper.PingData(c))
			info := &relaycommon.RelayInfo{IsStream: !test.nonStream, StreamStatus: relaycommon.NewStreamStatus()}
			if !test.legacyEOF {
				info.StreamStatus.RequireTerminal()
			}
			info.StreamStatus.SetEndReason(test.end, errors.New("private upstream detail"))
			switch test.terminal {
			case "completed":
				info.StreamStatus.MarkCompleted()
			case "failed":
				info.StreamStatus.MarkFailed("", "", 500)
			case "incomplete":
				info.StreamStatus.MarkIncomplete("max_output_tokens")
			}
			if test.cancelled {
				cancel()
			}
			service.MarkRequestPolicySuccess(c, info.StreamStatus)
			if test.status != 0 {
				assert.False(t, service.RequestPolicy(c).Successful, "consume-log policy must not label a truncated stream successful")
			}
			if test.legacyEOF || test.terminal == "completed" {
				assert.True(t, service.RequestPolicy(c).Successful)
			}
			apiError := relayStreamFailure(c, info)
			if test.status == 0 {
				assert.Nil(t, apiError)
				return
			}
			require.NotNil(t, apiError)
			assert.Equal(t, test.status, apiError.StatusCode)
			assert.Equal(t, test.message, apiError.Error())
			assert.Equal(t, "stop", service.DecideRelayRetry(c, apiError, 1).Action)
			writeRelayStreamError(c, types.RelayFormatClaude, apiError)
			assert.Equal(t, http.StatusOK, recorder.Code, "committed SSE headers must remain unchanged")
			assert.Equal(t, 1, strings.Count(recorder.Body.String(), "event: error\n"))
			assert.Contains(t, recorder.Body.String(), test.message)
			assert.NotContains(t, recorder.Body.String(), "private upstream detail")
			assert.NotContains(t, recorder.Body.String(), "message_stop", "an interrupted stream must never get a fake completion")
			assert.False(t, service.RequestPolicy(c).Successful)
		})
	}
}
