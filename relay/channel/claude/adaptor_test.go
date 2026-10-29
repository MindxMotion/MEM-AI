package claude

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Exercise the real ordering: DTO conversion, disabled-field filtering, channel
// overrides, then transport. The only upstream is an in-process HTTP server.
func bedrockCompatibilityRoundTrip(t *testing.T, body, settings string, override map[string]any, passthrough bool) ([]byte, *relaycommon.RelayInfo, error, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	received := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		received <- data
		w.Header().Set("Content-Type", "application/json")
		_, err = io.WriteString(w, `{}`)
		assert.NoError(t, err)
	}))
	defer server.Close()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	defer common.CleanupBodyStorage(c)
	var original dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &original))
	info := &relaycommon.RelayInfo{
		OriginModelName: original.Model,
		RelayFormat:     types.RelayFormatClaude,
		Request:         &original,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiType:           constant.APITypeAnthropic,
			UpstreamModelName: original.Model,
			ChannelBaseUrl:    server.URL,
			ParamOverride:     override,
		},
	}
	require.NoError(t, common.UnmarshalJsonStr(settings, &info.ChannelOtherSettings))
	info.ChannelSetting.PassThroughBodyEnabled = passthrough
	adaptor := &Adaptor{}
	data := []byte(body)
	if !passthrough {
		request, err := common.DeepCopy(&original)
		require.NoError(t, err)
		require.NoError(t, helper.ModelMappedHelper(c, info, request))
		require.NoError(t, helper.ApplyReasoningModelSuffix(c, info, request))
		out, err := adaptor.ConvertClaudeRequest(c, info, request)
		if err != nil {
			return nil, info, err, false
		}
		data, err = common.Marshal(out)
		require.NoError(t, err)
		data, err = relaycommon.RemoveDisabledFields(data, info.ChannelOtherSettings, false)
		require.NoError(t, err)
		data, err = relaycommon.ApplyParamOverrideWithRelayInfo(data, info)
		require.NoError(t, err)
	}
	resp, err := adaptor.DoRequest(c, info, bytes.NewReader(data))
	if err == nil {
		require.NotNil(t, resp)
		require.NoError(t, resp.(*http.Response).Body.Close())
	}
	select {
	case data := <-received:
		return data, info, err, true
	default:
		return nil, info, err, false
	}
}

func TestBedrockCompatibilityEffortTimeline(t *testing.T) {
	const user = `{"role":"user","content":"new input"}`
	const assistant = `{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"lookup","input":{"n":9007199254740993}}]}`
	const toolResult = `{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"result"}]}`
	const high = `{"role":"system","content":[],"output_config":{"effort":"high"}}`
	const low = `{"role":"system","content":"","output_config":{"effort":"low"}}`
	const maxEffort = `{"role":"system","content":[],"output_config":{"effort":"max"}}`
	for _, tc := range []struct {
		name, messages, effort, remaining string
	}{
		{"direct switch", user + "," + high, "high", user},
		{"before next input", user + "," + assistant + "," + high + "," + user, "high", user + "," + assistant + "," + user},
		{"latest of multiple switches", high + "," + user + "," + assistant + "," + low + "," + user + "," + maxEffort, "max", user + "," + assistant + "," + user},
		{"pending after assistant", user + "," + high + "," + assistant + "," + low, "high", user + "," + assistant},
		{"tool result does not activate pending effort", user + "," + high + "," + assistant + "," + low + "," + toolResult, "high", user + "," + assistant + "," + toolResult},
		{"marker after tool result is pending", user + "," + high + "," + assistant + "," + toolResult + "," + low, "high", user + "," + assistant + "," + toolResult},
		{"next new input activates latest pending effort", user + "," + assistant + "," + high + "," + low + "," + toolResult + "," + user, "low", user + "," + assistant + "," + toolResult + "," + user},
		{"top level remains for pending marker", user + "," + assistant + "," + high, "medium", user + "," + assistant},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-opus-5-5","max_tokens":4096,"output_config":{"effort":"medium","format":{"type":"json_schema","schema":{"type":"object"}},"future":9007199254740993},"messages":[` + tc.messages + `]}`
			got, info, err, called := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, false)
			require.NoError(t, err)
			require.True(t, called)
			want := `{"model":"claude-opus-5-5","max_tokens":4096,"output_config":{"effort":"` + tc.effort + `","format":{"type":"json_schema","schema":{"type":"object"}},"future":9007199254740993},"messages":[` + tc.remaining + `]}`
			assert.JSONEq(t, want, string(got))
			assert.Contains(t, string(got), "9007199254740993")
			require.NotEmpty(t, info.ConversionDiagnostics())
			assert.Equal(t, "claude_bedrock_effort_flattened", info.ConversionDiagnostics()[0].Code)
			assert.Equal(t, tc.effort, info.GetReasoningEffort())
		})
	}
}

func TestBedrockCompatibilityPreservesPayloadAndNormalizesDisplay(t *testing.T) {
	for _, display := range []string{"absent", "", "summarized", "omitted", "detailed", "future-display"} {
		t.Run(display, func(t *testing.T) {
			body := `{"model":"claude-opus-5-5","max_tokens":4096,"temperature":0,"top_k":0,"stream":false,` +
				`"speed":"standard","system":[{"type":"text","text":"ordinary system","future":9007199254740993}],` +
				`"thinking":{"type":"enabled","budget_tokens":1024,"future":9007199254740993` +
				`,"display":"` + display + `"},"safeguards":{"mode":"required","future":{"n":9007199254740993}},` +
				`"future_top":{"n":9007199254740993},"tools":[{"name":"lookup","input_schema":{"type":"object","future":9007199254740993}}],` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"hello","future":9007199254740993}],"future_message":true}]}`
			if display == "absent" {
				body = strings.ReplaceAll(body, `,"display":"absent"`, "")
			}
			got, info, err, called := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, false)
			require.NoError(t, err)
			require.True(t, called)
			wantDisplay := display
			if display != "absent" && display != "summarized" && display != "omitted" {
				wantDisplay = "summarized"
				require.Len(t, info.ConversionDiagnostics(), 1)
				assert.Equal(t, "claude_bedrock_display_normalized", info.ConversionDiagnostics()[0].Code)
				if display != "" {
					assert.NotContains(t, info.ConversionDiagnostics()[0].Message, display)
				}
			} else {
				assert.Empty(t, info.ConversionDiagnostics())
			}
			// speed is still filtered by existing channel policy, without
			// decoding unrelated large integers through float64.
			want := strings.ReplaceAll(body, `"speed":"standard",`, "")
			want = strings.ReplaceAll(want, `"display":"`+display+`"`, `"display":"`+wantDisplay+`"`)
			assert.JSONEq(t, want, string(got))
			assert.Equal(t, strings.Count(want, "9007199254740993"), strings.Count(string(got), "9007199254740993"))
		})
	}
}

func TestBedrockCompatibilityInitialOutputConfig(t *testing.T) {
	const user = `{"role":"user","content":"new input"}`
	const assistant = `{"role":"assistant","content":"done"}`
	const marker = `{"role":"system","content":[],"output_config":{"effort":"medium"}}`
	for _, tc := range []struct {
		name, initial, messages, output string
	}{
		{"absent initial active marker", "", user + "," + marker, `"output_config":{"effort":"medium"},`},
		{"absent initial pending marker", "", user + "," + assistant + "," + marker, ""},
		{"format only initial", `"output_config":{"format":{"type":"json_schema","future":9007199254740993}},`, user + "," + marker,
			`"output_config":{"format":{"type":"json_schema","future":9007199254740993},"effort":"medium"},`},
		{"null initial effort remains while pending", `"output_config":{"effort":null,"future":9007199254740993},`, user + "," + assistant + "," + marker,
			`"output_config":{"effort":null,"future":9007199254740993},`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-opus-5-5","max_tokens":64,` + tc.initial + `"safeguards":{"mode":"required","future":9007199254740993},"messages":[` + tc.messages + `]}`
			got, _, err, _ := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, false)
			require.NoError(t, err)
			kept := user
			if strings.Contains(tc.messages, assistant) {
				kept += "," + assistant
			}
			want := `{"model":"claude-opus-5-5","max_tokens":64,` + tc.output + `"safeguards":{"mode":"required","future":9007199254740993},"messages":[` + kept + `]}`
			assert.JSONEq(t, want, string(got))
			assert.Equal(t, strings.Count(want, "9007199254740993"), strings.Count(string(got), "9007199254740993"))
		})
	}
}

func TestBedrockCompatibilityRejectsAmbiguousRequests(t *testing.T) {
	for _, tc := range []struct{ name, fields, messages string }{
		{"ordinary system content", "", `{"role":"system","content":"do not discard"}`},
		{"content plus effort", "", `{"role":"system","content":[{"type":"text","text":"important"}],"output_config":{"effort":"high"}}`},
		{"unknown marker config", "", `{"role":"system","content":[],"output_config":{"effort":"high","future":true}}`},
		{"unknown marker field", "", `{"role":"system","content":[],"output_config":{"effort":"high"},"clear_at":"never"}`},
		{"user config", "", `{"role":"user","content":"","output_config":{"effort":"high"}}`},
		{"assistant config", "", `{"role":"assistant","content":"","output_config":{"effort":"high"}}`},
		{"unsupported role", "", `{"role":"developer","content":"important"}`},
		{"empty config", "", `{"role":"system","content":[],"output_config":{}}`},
		{"null effort", "", `{"role":"system","content":[],"output_config":{"effort":null}}`},
		{"unknown effort", "", `{"role":"system","content":[],"output_config":{"effort":"future"}}`},
		{"missing content", "", `{"role":"system","output_config":{"effort":"high"}}`},
		{"null content", "", `{"role":"system","content":null,"output_config":{"effort":"high"}}`},
		{"empty text block is not empty content", "", `{"role":"system","content":[{"type":"text","text":""}],"output_config":{"effort":"high"}}`},
		{"nonobject top config", `"output_config":[],`, `{"role":"system","content":[],"output_config":{"effort":"high"}}`},
		{"unknown initial effort", `"output_config":{"effort":"future"},`, `{"role":"system","content":[],"output_config":{"effort":"high"}}`},
		{"thinking disabled", `"thinking":{"type":"disabled"},`, `{"role":"system","content":[],"output_config":{"effort":"high"}}`},
		{"consecutive immediate markers", "", `{"role":"system","content":[],"output_config":{"effort":"high"}},{"role":"system","content":[],"output_config":{"effort":"low"}}`},
		{"duplicate config field", "", `{"role":"system","content":[],"output_config":{"effort":"high","effort":"low"}}`},
		{"unknown user block with marker", "", `{"role":"user","content":[{"type":"future_block"}]},{"role":"system","content":[],"output_config":{"effort":"high"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"claude-opus-5-5","max_tokens":4096,` + tc.fields +
				`"messages":[{"role":"user","content":"hello"},` + tc.messages + `]}`
			_, info, err, called := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, false)
			require.Error(t, err)
			assert.False(t, called)
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
			assert.True(t, types.IsSkipRetryError(apiErr))
			assert.NotEmpty(t, info.ConversionDiagnostics())
			assert.NotContains(t, err.Error(), "important")
		})
	}
}

func TestBedrockCompatibilityRunsAfterOverridesAndRejectsPassthrough(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":4096,"thinking":{"type":"adaptive","display":"summarized"},"messages":[{"role":"user","content":"hello"}]}`
	override := map[string]any{
		"thinking": map[string]any{"type": "adaptive", "display": "detailed"},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "system", "content": []any{}, "output_config": map[string]any{"effort": "xhigh"}},
		},
	}
	got, info, err, called := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, override, false)
	require.NoError(t, err)
	assert.True(t, called)
	assert.JSONEq(t, `{"model":"claude-opus-5-5","max_tokens":4096,"thinking":{"type":"adaptive","display":"summarized"},"output_config":{"effort":"xhigh"},"messages":[{"role":"user","content":"hello"}]}`, string(got))
	assert.Len(t, info.ConversionDiagnostics(), 2)

	for _, invalidDisplay := range []any{nil, 42} {
		override = map[string]any{"thinking": map[string]any{"type": "adaptive", "display": invalidDisplay}}
		_, _, err, called = bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, override, false)
		require.Error(t, err)
		assert.False(t, called)
	}
	override = map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello", "output_config": map[string]any{"effort": "high"}}}}
	_, _, err, called = bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, override, false)
	require.Error(t, err)
	assert.False(t, called)

	_, _, err, called = bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, true)
	require.Error(t, err)
	assert.False(t, called)
}

func TestBedrockCompatibilityPreservesDefaultsAndThinkingSuffix(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-5", "claude-3-7-sonnet-thinking"} {
		t.Run(model, func(t *testing.T) {
			body := `{"model":"` + model + `","max_tokens":0,"output_config":{"future":9007199254740993},"thinking":{"type":"enabled","budget_tokens":1024,"future":9007199254740993},"messages":[{"role":"user","content":"hello"}]}`
			native, _, err, _ := bedrockCompatibilityRoundTrip(t, body, `{}`, nil, false)
			require.NoError(t, err)
			compatible, _, err, _ := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, false)
			require.NoError(t, err)
			var nativeRequest, compatibleRequest dto.ClaudeRequest
			require.NoError(t, common.Unmarshal(native, &nativeRequest))
			require.NoError(t, common.Unmarshal(compatible, &compatibleRequest))
			assert.Equal(t, nativeRequest.Model, compatibleRequest.Model)
			assert.Equal(t, nativeRequest.MaxTokens, compatibleRequest.MaxTokens)
			assert.Equal(t, nativeRequest.Thinking, compatibleRequest.Thinking)
			assert.Contains(t, string(compatibleRequest.OutputConfig), "9007199254740993")
			assert.Equal(t, 2, strings.Count(string(compatible), `"future":9007199254740993`))
		})
	}
}

func TestBedrockCompatibilityPreservesSystemPromptOverrideFields(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":64,"system":[{"type":"text","text":"keep","future":9007199254740993}],"messages":[{"role":"user","content":"hello"}]}`
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	defer common.CleanupBodyStorage(c)
	var original dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &original))
	request, err := common.DeepCopy(&original)
	require.NoError(t, err)
	// This is the host's supported edit: prepend a channel system prompt.
	channelPrompt, content := "channel", "keep"
	request.System = []dto.ClaudeMediaMessage{{Type: "text", Text: &channelPrompt}, {Type: "text", Text: &content}}
	info := &relaycommon.RelayInfo{
		Request: &original,
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiType:              constant.APITypeAnthropic,
			ChannelOtherSettings: dto.ChannelOtherSettings{ClaudeBedrockCompat: true},
			ChannelSetting:       dto.ChannelSettings{SystemPrompt: "channel", SystemPromptOverride: true},
		},
	}
	out, err := (&Adaptor{}).ConvertClaudeRequest(c, info, request)
	require.NoError(t, err)
	encoded, err := common.Marshal(out)
	require.NoError(t, err)
	assert.JSONEq(t, `{"model":"claude-opus-5-5","max_tokens":64,"system":[{"type":"text","text":"channel"},{"type":"text","text":"keep","future":9007199254740993}],"messages":[{"role":"user","content":"hello"}]}`, string(encoded))
	assert.Contains(t, string(encoded), "9007199254740993")
}

func TestBedrockCompatibilityRejectsGlobalBypasses(t *testing.T) {
	global := model_setting.GetGlobalSettings()
	saved := *global
	t.Cleanup(func() { *global = saved })
	body := `{"model":"claude-opus-5-5","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`
	for _, mode := range []string{"global passthrough", "Responses routing"} {
		t.Run(mode, func(t *testing.T) {
			*global = saved
			if mode == "global passthrough" {
				global.PassThroughRequestEnabled = true
			} else {
				global.ChatCompletionsToResponsesPolicy = model_setting.ChatCompletionsToResponsesPolicy{
					Enabled: true, AllChannels: true, ModelPatterns: []string{".*"},
				}
			}
			_, _, err, called := bedrockCompatibilityRoundTrip(t, body, `{"claude_bedrock_compat":true}`, nil, false)
			require.Error(t, err)
			assert.False(t, called)
			var apiErr *types.NewAPIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.StatusCode)
			assert.True(t, types.IsSkipRetryError(apiErr))
		})
	}
}

func TestBedrockCompatibilityOffPreservesNativeContract(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":4096,"thinking":{"type":"adaptive","display":"detailed"},"safeguards":{"mode":"required"},"output_config":{"effort":"medium"},"messages":[{"role":"user","content":"hello"},{"role":"system","content":[],"output_config":{"effort":"high","future":true}}]}`
	for _, settings := range []string{`{}`, `{"claude_bedrock_compat":false}`} {
		got, info, err, called := bedrockCompatibilityRoundTrip(t, body, settings, nil, false)
		require.NoError(t, err)
		assert.True(t, called)
		assert.JSONEq(t, body, string(got))
		assert.Empty(t, info.ConversionDiagnostics())
	}
	got, info, err, called := bedrockCompatibilityRoundTrip(t, body, `{}`, nil, true)
	require.NoError(t, err)
	assert.True(t, called)
	assert.Equal(t, body, string(got))
	assert.Empty(t, info.ConversionDiagnostics())
}

func TestConvertClaudeRequestTreatsZeroMaxTokensAsUnset(t *testing.T) {
	zero := uint(0)
	req := &dto.ClaudeRequest{
		Model:     "claude-sonnet-4-5",
		MaxTokens: &zero,
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-sonnet-4-5",
		},
	}

	out, err := (&Adaptor{}).ConvertClaudeRequest(nil, info, req)
	require.NoError(t, err)
	converted, ok := out.(*dto.ClaudeRequest)
	require.True(t, ok)
	require.NotNil(t, converted.MaxTokens)
	assert.Equal(t, uint(model_setting.GetClaudeSettings().GetDefaultMaxTokens(req.Model)), *converted.MaxTokens)
}

func TestConvertClaudeRequestPreservesNativeClaudeCodeThinking(t *testing.T) {
	budget := 10000
	maxTokens := uint(20000)
	temperature := 0.7
	topP := 0.9
	req := &dto.ClaudeRequest{
		Model:        "claude-opus-4-8",
		MaxTokens:    &maxTokens,
		Temperature:  &temperature,
		TopP:         &topP,
		Thinking:     &dto.Thinking{Type: "enabled", BudgetTokens: &budget},
		OutputConfig: []byte(`{"effort":"high"}`),
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	info := &relaycommon.RelayInfo{
		OriginModelName: req.Model,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: req.Model,
		},
	}

	out, err := (&Adaptor{}).ConvertClaudeRequest(nil, info, req)
	require.NoError(t, err)
	converted, ok := out.(*dto.ClaudeRequest)
	require.True(t, ok)
	require.NotNil(t, converted.Thinking)
	assert.Equal(t, "enabled", converted.Thinking.Type)
	require.NotNil(t, converted.Thinking.BudgetTokens)
	assert.Equal(t, budget, *converted.Thinking.BudgetTokens)
	assert.Equal(t, temperature, *converted.Temperature)
	assert.Equal(t, topP, *converted.TopP)
	assert.JSONEq(t, `{"effort":"high"}`, string(converted.OutputConfig))
	assert.Empty(t, info.ConversionDiagnostics())
}

func TestConvertClaudeRequestPreservesMessageOutputConfig(t *testing.T) {
	body := `{"model":"claude-opus-5-5","max_tokens":64,"output_config":{"effort":"medium"},"messages":[` +
		`{"role":"user","content":"summary"},` +
		`{"role":"system","content":[],"output_config":{"effort":"high"}},` +
		`{"role":"assistant","content":"done"}]}`
	var req dto.ClaudeRequest
	require.NoError(t, common.UnmarshalJsonStr(body, &req))
	info := &relaycommon.RelayInfo{
		OriginModelName: req.Model,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: req.Model,
		},
	}

	out, err := (&Adaptor{}).ConvertClaudeRequest(nil, info, &req)
	require.NoError(t, err)
	encoded, err := common.Marshal(out)
	require.NoError(t, err)

	var upstream struct {
		Messages []map[string]any `json:"messages"`
	}
	require.NoError(t, common.Unmarshal(encoded, &upstream))
	require.Len(t, upstream.Messages, 3)
	assert.Equal(t, map[string]any{"effort": "high"}, upstream.Messages[1]["output_config"])
	assert.NotContains(t, upstream.Messages[0], "output_config")
	assert.NotContains(t, upstream.Messages[2], "output_config")
}

func TestConvertClaudeRequestZeroMaxTokensStillRaisesThinkingBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())

	zero := uint(0)
	original := &dto.ClaudeRequest{
		Model:     "claude-3-7-sonnet-thinking",
		MaxTokens: &zero,
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	info := &relaycommon.RelayInfo{
		OriginModelName: "claude-3-7-sonnet-thinking",
		Request:         original,
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-3-7-sonnet-thinking",
		},
	}
	outbound, err := common.DeepCopy(original)
	require.NoError(t, err)
	require.NoError(t, helper.ModelMappedHelper(c, info, outbound))
	err = helper.ApplyReasoningModelSuffix(nil, info, outbound)
	require.NoError(t, err)

	out, err := (&Adaptor{}).ConvertClaudeRequest(nil, info, outbound)
	require.NoError(t, err)
	converted, ok := out.(*dto.ClaudeRequest)
	require.True(t, ok)
	assert.Equal(t, "claude-3-7-sonnet", converted.Model)
	require.NotNil(t, converted.Thinking)
	require.NotNil(t, converted.MaxTokens)
	assert.Greater(t, *converted.MaxTokens, uint(1024))
}

func TestConvertClaudeRequestDoesNotOverwriteTrimmedUpstreamModelName(t *testing.T) {
	req := &dto.ClaudeRequest{
		Model: "claude-3-7-sonnet-thinking",
		Messages: []dto.ClaudeMessage{
			{Role: "user", Content: "hello"},
		},
	}
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-3-7-sonnet",
		},
	}

	_, err := (&Adaptor{}).ConvertClaudeRequest(nil, info, req)
	require.NoError(t, err)
	assert.Equal(t, "claude-3-7-sonnet", info.UpstreamModelName)
}

func geminiToClaudeInfo() *relaycommon.RelayInfo {
	return &relaycommon.RelayInfo{
		OriginModelName: "claude-3-7-sonnet",
		ChannelMeta: &relaycommon.ChannelMeta{
			UpstreamModelName: "claude-3-7-sonnet",
		},
	}
}

func TestConvertGeminiRequestMapsSystemInstructionToolsAndMultimodal(t *testing.T) {
	req := &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{
			{
				Role: "user",
				Parts: []dto.GeminiPart{
					{Text: "What is in this image?"},
					{InlineData: &dto.GeminiInlineData{MimeType: "image/png", Data: "aGVsbG8="}},
				},
			},
		},
		SystemInstructions: &dto.GeminiChatContent{
			Parts: []dto.GeminiPart{{Text: "You are a helpful assistant."}},
		},
	}
	req.SetTools([]dto.GeminiChatTool{
		{
			FunctionDeclarations: []dto.FunctionRequest{
				{
					Name:        "lookup",
					Description: "Lookup data",
					Parameters: map[string]any{
						"type":       "object",
						"properties": map[string]any{"q": map[string]any{"type": "string"}},
					},
				},
			},
		},
	})

	out, err := (&Adaptor{}).ConvertGeminiRequest(nil, geminiToClaudeInfo(), req)
	require.NoError(t, err)
	converted, ok := out.(*dto.ClaudeRequest)
	require.True(t, ok)

	system := converted.ParseSystem()
	require.NotEmpty(t, system)
	assert.Contains(t, system[0].GetText(), "You are a helpful assistant.")
	require.NotEmpty(t, converted.Messages)
	assert.Equal(t, "user", converted.Messages[0].Role)

	blocks, parseErr := converted.Messages[0].ParseContent()
	require.NoError(t, parseErr)
	var foundImage bool
	for _, block := range blocks {
		if block.Type == "image" || (block.Source != nil && block.Source.Type == "base64") {
			foundImage = true
			break
		}
	}
	assert.True(t, foundImage)

	require.NotNil(t, converted.Tools)
	tools, err := common.Marshal(converted.Tools)
	require.NoError(t, err)
	assert.Contains(t, string(tools), `"lookup"`)
	require.NotNil(t, converted.MaxTokens)
	assert.Greater(t, *converted.MaxTokens, uint(0))
}

func TestConvertGeminiRequestThinkingConfigUsesReasoningIntent(t *testing.T) {
	budget := 1024
	maxTokens := uint(4096)
	req := &dto.GeminiChatRequest{
		Contents: []dto.GeminiChatContent{
			{Role: "user", Parts: []dto.GeminiPart{{Text: "think"}}},
		},
		GenerationConfig: dto.GeminiChatGenerationConfig{
			MaxOutputTokens: &maxTokens,
			ThinkingConfig:  &dto.GeminiThinkingConfig{ThinkingBudget: &budget},
		},
	}

	out, err := (&Adaptor{}).ConvertGeminiRequest(nil, geminiToClaudeInfo(), req)
	require.NoError(t, err)
	converted, ok := out.(*dto.ClaudeRequest)
	require.True(t, ok)
	require.NotNil(t, converted.Thinking)
	assert.Equal(t, "enabled", converted.Thinking.Type)
	require.NotNil(t, converted.Thinking.BudgetTokens)
	assert.Equal(t, 1024, *converted.Thinking.BudgetTokens)
}

func TestConvertGeminiRequestNilRequest(t *testing.T) {
	_, err := (&Adaptor{}).ConvertGeminiRequest(nil, geminiToClaudeInfo(), nil)
	require.Error(t, err)
}
