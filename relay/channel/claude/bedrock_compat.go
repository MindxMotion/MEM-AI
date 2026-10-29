package claude

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/model_setting"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ValidateBedrockCompatibility is also called before handlers can bypass the
// Claude adaptor through passthrough or Chat Completions -> Responses routing.
func ValidateBedrockCompatibility(c *gin.Context, info *relaycommon.RelayInfo) error {
	if info == nil || info.ChannelMeta == nil || !info.ChannelOtherSettings.ClaudeBedrockCompat {
		return nil
	}
	if info.ApiType != constant.APITypeAnthropic {
		return bedrockCompatibilityError(c, info, "claude_bedrock_compat", "The compatibility profile requires the Claude Messages adaptor.")
	}
	if model_setting.GetGlobalSettings().PassThroughRequestEnabled || info.ChannelSetting.PassThroughBodyEnabled {
		return bedrockCompatibilityError(c, info, "pass_through_body_enabled", "Body passthrough cannot be combined with request compatibility transformations.")
	}
	if (info.RelayFormat == types.RelayFormatClaude || info.RelayFormat == types.RelayFormatOpenAI) &&
		service.ShouldChatCompletionsUseResponsesGlobal(info.ChannelId, info.ChannelType, info.OriginModelName) {
		return bedrockCompatibilityError(c, info, "claude_bedrock_compat", "Responses routing bypasses the Claude Messages compatibility profile.")
	}
	return nil
}

func bedrockCompatibilityDiagnostic(code, path, message string, severity types.ConversionDiagnosticSeverity) types.ConversionDiagnostic {
	return types.ConversionDiagnostic{
		Code: code, Path: path, Message: message, Severity: severity,
		From: types.RelayFormatClaude, To: types.RelayFormatClaude,
	}
}

func bedrockCompatibilityError(c *gin.Context, info *relaycommon.RelayInfo, path, message string) error {
	info.RecordConversionDiagnostics(c, []types.ConversionDiagnostic{
		bedrockCompatibilityDiagnostic("claude_bedrock_unsupported", path, message, types.ConversionDiagnosticError),
	})
	return types.NewErrorWithStatusCode(
		errors.New(i18n.T(c, i18n.MsgRelayClaudeBedrockUnsupported, map[string]any{"Field": path})),
		types.ErrorCodeConvertRequestFailed, http.StatusBadRequest, types.ErrOptionWithSkipRetry(),
	)
}

// Maps of RawMessage retain unknown fields, explicit zeroes and integer precision.
// Reject duplicate keys in objects whose semantics we interpret, rather than
// guessing whether a downstream JSON decoder uses the first or the last value.
func bedrockClaudeObject(data []byte) (map[string]common.RawMessage, error) {
	var object map[string]common.RawMessage
	if err := common.Unmarshal(data, &object); err != nil || object == nil {
		return nil, errors.New("expected a JSON object")
	}
	seen := make(map[string]struct{}, len(object))
	duplicate := false
	gjson.ParseBytes(data).ForEach(func(key, _ gjson.Result) bool {
		name := key.String()
		if _, exists := seen[name]; exists {
			duplicate = true
			return false
		}
		seen[name] = struct{}{}
		return true
	})
	if duplicate {
		return nil, errors.New("duplicate JSON object keys")
	}
	return object, nil
}

// Native DTO conversion owns model/system/thinking/max_tokens changes, but does
// not own unknown client fields. Apply its delta to the original JSON only for
// this opt-in profile. In particular, never rebuild messages/tool blocks from
// DTOs or decode their numbers through map[string]any.
func preserveBedrockClaudeFields(c *gin.Context, info *relaycommon.RelayInfo, request *dto.ClaudeRequest) (any, error) {
	_, native := info.Request.(*dto.ClaudeRequest)
	if !native || c == nil || c.Request == nil {
		return request, nil
	}
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, bedrockCompatibilityError(c, info, "$", "The original request body is required to preserve unknown fields.")
	}
	raw, err := storage.Bytes()
	if err != nil {
		return nil, bedrockCompatibilityError(c, info, "$", "The original request body could not be read.")
	}
	payload, err := bedrockClaudeObject(raw)
	if err != nil {
		return nil, bedrockCompatibilityError(c, info, "$", "A JSON object with unique keys is required.")
	}
	// Host model normalization also updates info.Request, so the baseline for
	// this delta must be decoded from the immutable client body, not that DTO.
	var original dto.ClaudeRequest
	if err := common.Unmarshal(raw, &original); err != nil {
		return nil, bedrockCompatibilityError(c, info, "$", "The original Claude request cannot be decoded safely.")
	}
	beforeJSON, err := common.Marshal(&original)
	if err != nil {
		return nil, err
	}
	afterJSON, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	before, err := bedrockClaudeObject(beforeJSON)
	if err != nil {
		return nil, err
	}
	after, err := bedrockClaudeObject(afterJSON)
	if err != nil {
		return nil, err
	}
	for key, value := range after {
		if bytes.Equal(before[key], value) {
			continue
		}
		var ownedFields []string
		switch key {
		case "thinking":
			ownedFields = []string{"type", "budget_tokens", "display"}
		case "output_config":
			// The reasoning suffix renderer owns effort, not output format or
			// unknown config fields (which its DTO roundtrip may re-encode).
			ownedFields = []string{"effort"}
		case "system":
			if rawSystem, exists := payload[key]; exists && common.GetJsonType(rawSystem) != "string" &&
				!bytes.Equal(bytes.TrimSpace(rawSystem), []byte("null")) {
				var blocks []common.RawMessage
				if common.GetJsonType(rawSystem) != "array" || common.Unmarshal(rawSystem, &blocks) != nil ||
					!info.ChannelSetting.SystemPromptOverride || info.ChannelSetting.SystemPrompt == "" {
					return nil, bedrockCompatibilityError(c, info, "system", "A structured system prompt change cannot be preserved safely.")
				}
				prompt, err := common.Marshal(map[string]string{"type": "text", "text": info.ChannelSetting.SystemPrompt})
				if err != nil {
					return nil, err
				}
				value, err = common.Marshal(append([]common.RawMessage{prompt}, blocks...))
				if err != nil {
					return nil, err
				}
			}
		}
		if len(ownedFields) > 0 {
			current := make(map[string]common.RawMessage)
			if raw, exists := payload[key]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				current, err = bedrockClaudeObject(raw)
				if err != nil {
					return nil, bedrockCompatibilityError(c, info, key, "Configuration changes require an object with unique keys.")
				}
			}
			previous := make(map[string]common.RawMessage)
			if raw, exists := before[key]; exists {
				previous, err = bedrockClaudeObject(raw)
				if err != nil {
					return nil, err
				}
			}
			updated, err := bedrockClaudeObject(value)
			if err != nil {
				return nil, err
			}
			for _, name := range ownedFields {
				if bytes.Equal(previous[name], updated[name]) {
					continue
				}
				if v, exists := updated[name]; exists {
					current[name] = v
				} else {
					delete(current, name)
				}
			}
			value, err = common.Marshal(current)
			if err != nil {
				return nil, err
			}
		}
		payload[key] = value
	}
	for key := range before {
		if _, exists := after[key]; !exists {
			delete(payload, key)
		}
	}
	return payload, nil
}

func prepareBedrockClaudeJSON(c *gin.Context, info *relaycommon.RelayInfo, data []byte) ([]byte, error) {
	payload, err := bedrockClaudeObject(data)
	if err != nil {
		return nil, bedrockCompatibilityError(c, info, "$", "A JSON object with unique keys is required.")
	}
	var messages []common.RawMessage
	if err := common.Unmarshal(payload["messages"], &messages); err != nil || len(messages) == 0 {
		return nil, bedrockCompatibilityError(c, info, "messages", "A nonempty Messages conversation is required.")
	}
	// First validate every marker. No partial transformations on rejection.
	objects := make([]map[string]common.RawMessage, len(messages))
	efforts := make([]common.RawMessage, len(messages))
	markerCount := 0
	for i, raw := range messages {
		path := fmt.Sprintf("messages[%d]", i)
		message, err := bedrockClaudeObject(raw)
		if err != nil {
			return nil, bedrockCompatibilityError(c, info, path, "Message objects must have unique keys.")
		}
		objects[i] = message
		var role string
		if err := common.Unmarshal(message["role"], &role); err != nil || !slices.Contains([]string{"system", "user", "assistant"}, role) {
			return nil, bedrockCompatibilityError(c, info, path+".role", "The message role cannot be converted safely.")
		}
		config, hasConfig := message["output_config"]
		if role != "system" {
			if hasConfig {
				return nil, bedrockCompatibilityError(c, info, path+".output_config", "Only effort-only system markers support per-message output configuration.")
			}
			continue
		}
		if !hasConfig || len(message) != 3 {
			return nil, bedrockCompatibilityError(c, info, path, "Only system markers containing role, empty content and effort-only output_config can be converted.")
		}
		content := bytes.TrimSpace(message["content"])
		empty := bytes.Equal(content, []byte(`""`))
		if len(content) > 0 && content[0] == '[' {
			var blocks []common.RawMessage
			empty = common.Unmarshal(content, &blocks) == nil && len(blocks) == 0
		}
		if !empty {
			return nil, bedrockCompatibilityError(c, info, path+".content", "System content is not discarded or relocated by this profile.")
		}
		output, err := bedrockClaudeObject(config)
		if err != nil || len(output) != 1 || !bedrockClaudeEffort(output["effort"]) {
			return nil, bedrockCompatibilityError(c, info, path+".output_config", "A marker must contain only a recognized non-null effort level.")
		}
		efforts[i] = output["effort"]
		markerCount++
	}

	var diagnostics []types.ConversionDiagnostic
	if markerCount > 0 {
		output := make(map[string]common.RawMessage)
		if raw, exists := payload["output_config"]; exists {
			output, err = bedrockClaudeObject(raw)
			if err != nil {
				return nil, bedrockCompatibilityError(c, info, "output_config", "Top-level output configuration must be an object.")
			}
			if effort, exists := output["effort"]; exists && !bytes.Equal(bytes.TrimSpace(effort), []byte("null")) && !bedrockClaudeEffort(effort) {
				return nil, bedrockCompatibilityError(c, info, "output_config.effort", "The initial effort level is not recognized.")
			}
		}
		if thinking, exists := payload["thinking"]; exists && !bytes.Equal(bytes.TrimSpace(thinking), []byte("null")) {
			object, err := bedrockClaudeObject(thinking)
			if err != nil {
				return nil, bedrockCompatibilityError(c, info, "thinking", "Thinking configuration must be an object.")
			}
			var kind string
			if err := common.Unmarshal(object["type"], &kind); err != nil || (kind != "adaptive" && kind != "enabled") {
				return nil, bedrockCompatibilityError(c, info, "thinking.type", "Effort switches require enabled or adaptive thinking when thinking is specified.")
			}
		}

		// Official effort.md: top-level is the initial effort. A marker directly
		// after fresh user input applies to that reply; elsewhere it is pending
		// until the next fresh user input. Tool-result-only turns do not count.
		// https://platform.claude.com/docs/en/build-with-claude/effort#change-effort-mid-conversation
		var active, pending common.RawMessage
		lastWasFreshUser := false
		immediateMarker := false
		kept := make([]common.RawMessage, 0, len(messages)-markerCount)
		for i, message := range objects {
			if efforts[i] != nil {
				if immediateMarker {
					return nil, bedrockCompatibilityError(c, info, fmt.Sprintf("messages[%d]", i), "Consecutive markers immediately after fresh user input have ambiguous activation timing.")
				}
				if lastWasFreshUser {
					active = efforts[i]
					immediateMarker = true
				} else {
					pending = efforts[i]
				}
				lastWasFreshUser = false
				continue
			}
			immediateMarker = false
			var role string
			_ = common.Unmarshal(message["role"], &role) // validated above
			lastWasFreshUser = false
			if role == "user" {
				fresh, err := bedrockClaudeFreshInput(message["content"])
				if err != nil {
					return nil, bedrockCompatibilityError(c, info, fmt.Sprintf("messages[%d].content", i), "User input cannot be classified safely for effort switching.")
				}
				lastWasFreshUser = fresh
				if fresh && pending != nil {
					active, pending = pending, nil
				}
			}
			kept = append(kept, messages[i])
		}
		if len(kept) == 0 {
			return nil, bedrockCompatibilityError(c, info, "messages", "Effort markers alone are not a conversation.")
		}
		if active != nil {
			output["effort"] = active
			payload["output_config"], err = common.Marshal(output)
			if err != nil {
				return nil, err
			}
			var effort string
			_ = common.Unmarshal(active, &effort)
			info.SetReasoningEffort(effort)
		}
		payload["messages"], err = common.Marshal(kept)
		if err != nil {
			return nil, err
		}
		diagnostics = append(diagnostics, bedrockCompatibilityDiagnostic(
			"claude_bedrock_effort_flattened", "messages",
			"Effort-only system markers were removed; only the currently effective effort is forwarded. Historical effort and prompt-cache semantics cannot be represented.",
			types.ConversionDiagnosticWarning,
		))
	}
	if raw, exists := payload["thinking"]; exists && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		thinking, err := bedrockClaudeObject(raw)
		if err != nil {
			return nil, bedrockCompatibilityError(c, info, "thinking", "Thinking configuration must be an object.")
		}
		if rawDisplay, exists := thinking["display"]; exists {
			var display string
			if bytes.Equal(bytes.TrimSpace(rawDisplay), []byte("null")) || common.Unmarshal(rawDisplay, &display) != nil {
				return nil, bedrockCompatibilityError(c, info, "thinking.display", "Thinking display must be a string.")
			}
			if display != "summarized" && display != "omitted" {
				thinking["display"] = common.RawMessage(`"summarized"`)
				payload["thinking"], err = common.Marshal(thinking)
				if err != nil {
					return nil, err
				}
				diagnostics = append(diagnostics, bedrockCompatibilityDiagnostic(
					"claude_bedrock_display_normalized", "thinking.display",
					"An unsupported thinking display was normalized to summarized; thinking type and budget were not changed.",
					types.ConversionDiagnosticWarning,
				))
			}
		}
	}
	result, err := common.Marshal(payload)
	if err != nil {
		return nil, err
	}
	info.RecordConversionDiagnostics(c, diagnostics)
	return result, nil
}

func bedrockClaudeEffort(raw common.RawMessage) bool {
	var effort string
	return common.Unmarshal(raw, &effort) == nil && slices.Contains([]string{"low", "medium", "high", "xhigh", "max"}, effort)
}

func bedrockClaudeFreshInput(raw common.RawMessage) (bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '"' {
		var text string
		err := common.Unmarshal(raw, &text)
		return text != "", err
	}
	var blocks []common.RawMessage
	if err := common.Unmarshal(raw, &blocks); err != nil || blocks == nil {
		return false, errors.New("expected string or content blocks")
	}
	fresh := false
	for _, block := range blocks {
		object, err := bedrockClaudeObject(block)
		if err != nil {
			return false, err
		}
		var kind string
		if err := common.Unmarshal(object["type"], &kind); err != nil {
			return false, err
		}
		switch kind {
		case "tool_result":
		case "text", "image", "document", "search_result":
			fresh = true
		default:
			return false, errors.New("unrecognized user content block")
		}
	}
	return fresh, nil
}
