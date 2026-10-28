package service

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const relayDiagnosticContext = "relay_diagnostic"

type relayDiagnostic struct {
	features model.RelayOutcome
	info     *relaycommon.RelayInfo
	apiError *types.NewAPIError
}

func relayDiagnosticState(c *gin.Context) *relayDiagnostic {
	if value, ok := c.Get(relayDiagnosticContext); ok {
		return value.(*relayDiagnostic)
	}
	state := &relayDiagnostic{}
	c.Set(relayDiagnosticContext, state)
	return state
}

// CaptureRelayRequestFeatures reads only shape flags from the already validated
// JSON body. Values, tools, messages and prompt text are never retained.
func CaptureRelayRequestFeatures(c *gin.Context, body []byte) {
	if !constant.ErrorLogEnabled {
		return
	}
	state := relayDiagnosticState(c)
	state.features.FeaturesKnown = true
	state.features.RequestSize = "lt_4k"
	switch {
	case len(body) >= 1024*1024:
		state.features.RequestSize = "gte_1m"
	case len(body) >= 128*1024:
		state.features.RequestSize = "128k_1m"
	case len(body) >= 32*1024:
		state.features.RequestSize = "32k_128k"
	case len(body) >= 4*1024:
		state.features.RequestSize = "4k_32k"
	}
	values := gjson.GetManyBytes(body, "tools", "response_format.type", "thinking", "reasoning_effort", "reasoning", "stream", "text.format.type")
	state.features.HasTools = values[0].IsArray() && values[0].Get("0").Exists()
	state.features.Structured = values[1].String() == "json_object" || values[1].String() == "json_schema" ||
		values[6].String() == "json_object" || values[6].String() == "json_schema"
	state.features.Reasoning = values[2].Exists() || values[3].Exists() || values[4].Exists()
	state.features.Stream = values[5].Bool()
	for _, path := range []string{"messages", "input"} {
		gjson.GetBytes(body, path).ForEach(func(_, message gjson.Result) bool {
			content := message.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool {
					switch block.Get("type").String() {
					case "image", "image_url", "input_image":
						state.features.HasImages = true
					}
					return !state.features.HasImages
				})
			}
			return !state.features.HasImages
		})
		if state.features.HasImages {
			break
		}
	}
}

func SetRelayDiagnosticResult(c *gin.Context, info *relaycommon.RelayInfo, apiError *types.NewAPIError) {
	if !constant.ErrorLogEnabled {
		return
	}
	state := relayDiagnosticState(c)
	state.info, state.apiError = info, apiError
}

func RelayClientFamily(agent string) string {
	agent = strings.ToLower(agent[:min(len(agent), 512)])
	for _, family := range []struct {
		name  string
		terms []string
	}{
		{"workbuddy", []string{"workbuddy"}},
		{"claude-code", []string{"claude-cli", "claude-code"}},
		{"codex", []string{"codex_cli", "codex-cli"}},
		{"anthropic-sdk", []string{"anthropic"}},
		{"openai-sdk", []string{"openai"}},
		{"curl", []string{"curl/"}},
		{"python-urllib", []string{"python-urllib"}},
		{"browser", []string{"mozilla/"}},
	} {
		for _, term := range family.terms {
			if strings.Contains(agent, term) {
				return family.name
			}
		}
	}
	return "unknown"
}

func FinishRelayDiagnostic(c *gin.Context, endpoint string, started time.Time, panicked bool) {
	if !constant.ErrorLogEnabled {
		return
	}
	state := relayDiagnosticState(c)
	result := state.features
	result.RequestID = c.GetString(common.RequestIdKey)
	if result.RequestID == "" {
		result.RequestID = common.NewRequestId()
	}
	result.CreatedAt = time.Now().Unix()
	result.Endpoint = endpoint
	result.ClientFamily = RelayClientFamily(c.Request.UserAgent())
	result.ModelName = c.GetString("original_model")
	if len(result.ModelName) > 128 {
		result.ModelName = result.ModelName[:128]
		for !utf8.ValidString(result.ModelName) {
			result.ModelName = result.ModelName[:len(result.ModelName)-1]
		}
	}
	result.ChannelID = c.GetInt("channel_id")
	result.Authenticated = c.GetInt("id") > 0 && (c.GetInt("token_id") > 0 || c.GetString("username") != "")
	result.Origin = "api"
	if strings.HasPrefix(endpoint, "/pg/") {
		result.Origin = "playground"
	}
	result.HTTPStatus = c.Writer.Status()
	result.ResponseStarted = c.Writer.Written()
	if state.apiError != nil {
		result.ErrorStatus = state.apiError.StatusCode
	}
	result.Attempts = RequestPolicy(c).Attempts
	result.LatencyMS = time.Since(started).Milliseconds()
	result.FirstResponseMS = -1
	var stream *relaycommon.StreamStatus
	if state.info != nil {
		result.Stream = state.info.IsStream
		stream = state.info.StreamStatus
		if state.info.HasSendResponse() {
			result.FirstResponseMS = max(0, state.info.FirstResponseTime.Sub(state.info.StartTime).Milliseconds())
		}
	}
	if stream == nil {
		stream, _ = common.GetContextKeyType[*relaycommon.StreamStatus](c, constant.ContextKeyResponseStreamStatus)
	}
	snapshot := stream.OutcomeSnapshot()
	if result.ErrorStatus == 0 {
		result.ErrorStatus = snapshot.ErrorStatus
	}
	result.StreamEnd = string(snapshot.EndReason)
	result.Outcome, result.Reason = "success", "completed"
	switch {
	case panicked:
		result.Outcome, result.Reason = "failure", "panic"
		if !c.Writer.Written() {
			result.HTTPStatus = 500
		}
	case snapshot.Response == relaycommon.ResponseOutcomeFailed:
		result.Outcome, result.Reason = "failure", "upstream_stream_failed"
	case c.Request.Context().Err() == context.DeadlineExceeded,
		snapshot.EndReason == relaycommon.StreamEndReasonTimeout:
		result.Outcome, result.Reason = "failure", "timeout"
	case c.Request.Context().Err() == context.Canceled && snapshot.Response != relaycommon.ResponseOutcomeCompleted,
		snapshot.Response == relaycommon.ResponseOutcomeCancelled,
		snapshot.EndReason == relaycommon.StreamEndReasonClientGone && snapshot.Response != relaycommon.ResponseOutcomeCompleted:
		result.Outcome, result.Reason = "cancelled", "client_disconnected"
	case state.apiError != nil:
		result.Outcome, result.Reason = "failure", "relay_error"
		switch state.apiError.GetErrorCode() {
		case types.ErrorCodeModelNotFound, types.ErrorCodeGetChannelFailed:
			result.Reason = "channel_unavailable"
		case types.ErrorCodeConvertRequestFailed:
			result.Reason = "request_conversion_failed"
		case types.ErrorCodeDoRequestFailed:
			result.Reason = "upstream_transport_error"
		}
		if state.apiError.StatusCode >= 400 && state.apiError.StatusCode < 500 && state.apiError.StatusCode != 429 {
			result.Outcome, result.Reason = "rejected", "request_rejected"
		}
	case result.HTTPStatus >= 400:
		result.Outcome, result.Reason = "failure", "http_error"
		if result.HTTPStatus < 500 {
			result.Outcome, result.Reason = "rejected", "request_rejected"
		}
	case snapshot.HasErrors || snapshot.EndReason == relaycommon.StreamEndReasonScannerErr:
		result.Outcome, result.Reason = "failure", "stream_error"
	case snapshot.ExpectsTerminal && snapshot.Response == relaycommon.ResponseOutcomeUnknown:
		result.Outcome, result.Reason = "failure", "missing_terminal_event"
	case snapshot.Response == relaycommon.ResponseOutcomeIncomplete:
		result.Outcome, result.Reason = "incomplete", "upstream_incomplete"
	}
	model.RecordRelayOutcome(result)
}
