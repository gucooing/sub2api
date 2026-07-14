package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokComposerImageBridgeVisionModel     = "grok-build-0.1"
	grokComposerImageBridgeMaxOutputTokens = 512
	grokUpstreamUserAgent                  = "grok-shell/0.2.101"
	grokCLIVersion                         = "0.2.101"
	grokRateLimitFallbackCooldown          = 2 * time.Minute
	// Free-tier 429 bodies often omit Retry-After / x-ratelimit-reset-*.
	// xAI states free usage resets on a rolling 24-hour window.
	grokFreeUsageExhaustedCooldown = 24 * time.Hour
	grokFreeUsageExhaustedCode     = "subscription:free-usage-exhausted"
	grokFreeUsageExhaustedSource   = "free_usage_exhausted_body"
	grokFreeUsageExhaustedStatus   = "free_usage_exhausted"
)

func (s *OpenAIGatewayService) forwardGrokResponses(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	originalModel string,
	reqStream bool,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if account.Type != AccountTypeOAuth && account.Type != AccountTypeAPIKey {
		return nil, fmt.Errorf("grok account type %s is not supported by Responses forwarding", account.Type)
	}

	upstreamModel := account.GetMappedModel(originalModel)
	if strings.TrimSpace(upstreamModel) == "" {
		upstreamModel = "grok-4.3"
	}
	cacheIdentity := resolveGrokCacheIdentity(c, body, "", upstreamModel)
	patchedBody, err := patchGrokResponsesBody(body, upstreamModel)
	if err != nil {
		return nil, err
	}
	patchedBody, err = applyGrokResponsesCacheIdentity(patchedBody, body, cacheIdentity, account.IsGrokOAuth())
	if err != nil {
		return nil, fmt.Errorf("apply grok prompt cache identity: %w", err)
	}

	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	defer releaseUpstreamCtx()
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, patchedBody, token, cacheIdentity)
	if err != nil {
		return nil, err
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	upstreamStart := time.Now()
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	SetOpsLatencyMs(c, OpsUpstreamLatencyMsKey, time.Since(upstreamStart).Milliseconds())
	if err != nil {
		return nil, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI upstream returned status %d", resp.StatusCode)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleErrorResponse(ctx, resp, c, account, patchedBody, upstreamModel)
	}

	s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))

	var usage *OpenAIUsage
	var firstTokenMs *int
	responseID := ""
	if reqStream {
		streamResult, err := s.handleStreamingResponse(ctx, resp, c, account, startTime, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		responseID = strings.TrimSpace(streamResult.responseID)
	} else {
		nonStreamResult, err := s.handleNonStreamingResponse(ctx, resp, c, account, originalModel, upstreamModel)
		if err != nil {
			return nil, err
		}
		usage = nonStreamResult.usage
		responseID = strings.TrimSpace(nonStreamResult.responseID)
	}

	if usage == nil {
		usage = &OpenAIUsage{}
	}
	reasoningEffort := extractOpenAIReasoningEffortFromBody(patchedBody, originalModel)
	return &OpenAIForwardResult{
		RequestID:       firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
		ResponseID:      responseID,
		Usage:           *usage,
		Model:           originalModel,
		UpstreamModel:   upstreamModel,
		ReasoningEffort: reasoningEffort,
		Stream:          reqStream,
		OpenAIWSMode:    false,
		ResponseHeaders: resp.Header.Clone(),
		Duration:        time.Since(startTime),
		FirstTokenMs:    firstTokenMs,
	}, nil
}

func patchGrokResponsesBody(body []byte, upstreamModel string) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid json request body")
	}
	out, err := sjson.SetBytes(body, "model", upstreamModel)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesModelCapabilities(out, upstreamModel)
	if err != nil {
		return nil, err
	}
	for _, unsupportedField := range []string{"prompt_cache_retention", "safety_identifier"} {
		if gjson.GetBytes(out, unsupportedField).Exists() {
			out, err = sjson.DeleteBytes(out, unsupportedField)
			if err != nil {
				return nil, err
			}
		}
	}
	if strings.EqualFold(upstreamModel, "grok-4.5") {
		for _, unsupportedField := range []string{"presence_penalty", "presencePenalty", "frequency_penalty", "frequencyPenalty", "stop"} {
			if gjson.GetBytes(out, unsupportedField).Exists() {
				out, err = sjson.DeleteBytes(out, unsupportedField)
				if err != nil {
					return nil, err
				}
			}
		}
	}
	out, err = sanitizeGrokResponsesUnsupportedFields(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesInput(out)
	if err != nil {
		return nil, err
	}
	out, err = sanitizeGrokResponsesTools(out)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func sanitizeGrokResponsesModelCapabilities(body []byte, upstreamModel string) ([]byte, error) {
	if !grokModelRejectsReasoningEffort(upstreamModel) {
		return body, nil
	}

	out := body
	for _, field := range []string{"reasoning", "reasoning_effort", "reasoningEffort"} {
		if !gjson.GetBytes(out, field).Exists() {
			continue
		}
		var err error
		out, err = sjson.DeleteBytes(out, field)
		if err != nil {
			return nil, fmt.Errorf("remove unsupported Grok Composer %s: %w", field, err)
		}
	}
	return out, nil
}

func grokModelRejectsReasoningEffort(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if slash := strings.LastIndex(model, "/"); slash >= 0 {
		model = strings.TrimSpace(model[slash+1:])
	}
	switch model {
	case "grok-composer", "grok-composer-2.5-fast", "composer-2.5":
		return true
	default:
		return false
	}
}

var grokResponsesUnsupportedRecursiveFields = map[string]struct{}{
	"external_web_access": {},
}

func sanitizeGrokResponsesUnsupportedFields(body []byte) ([]byte, error) {
	if !bytes.Contains(body, []byte(`"external_web_access"`)) {
		return body, nil
	}

	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if !deleteJSONFields(payload, grokResponsesUnsupportedRecursiveFields) {
		return body, nil
	}
	return json.Marshal(payload)
}

func deleteJSONFields(value any, fields map[string]struct{}) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for field := range fields {
			if _, ok := typed[field]; ok {
				delete(typed, field)
				changed = true
			}
		}
		for _, child := range typed {
			if deleteJSONFields(child, fields) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if deleteJSONFields(child, fields) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

// Codex/Responses Lite emits input item types that xAI's Responses schema does
// not accept. Untagged ModelInput deserialization then fails with:
//
//	Failed to deserialize the JSON body into the target type:
//	data did not match any variant of untagged enum ModelInput
//
// We drop private carriers and rewrite custom/shell tool history into the
// ordinary function_call / function_call_output shape Grok understands.
// Top-level tools are handled separately by sanitizeGrokResponsesTools.
func sanitizeGrokResponsesInput(body []byte) ([]byte, error) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || !input.IsArray() {
		return body, nil
	}

	rawItems := input.Array()
	filtered := make([]json.RawMessage, 0, len(rawItems))
	changed := false
	for _, item := range rawItems {
		// Plain string input items are valid ModelInput entries.
		if item.Type == gjson.String {
			filtered = append(filtered, json.RawMessage(item.Raw))
			continue
		}
		if !item.IsObject() {
			changed = true
			continue
		}

		itemType := strings.TrimSpace(item.Get("type").String())
		switch itemType {
		case "additional_tools", "item_reference",
			"computer_call", "computer_call_output",
			"image_generation_call", "code_interpreter_call",
			"mcp_list_tools", "mcp_approval_request", "mcp_approval_response", "mcp_call":
			changed = true
			continue
		case "custom_tool_call", "local_shell_call", "tool_call":
			rewritten, ok := rewriteGrokUnsupportedToolCallInput(item, itemType)
			if !ok {
				changed = true
				continue
			}
			filtered = append(filtered, rewritten)
			changed = true
		case "custom_tool_call_output", "local_shell_call_output", "tool_call_output":
			rewritten, ok := rewriteGrokUnsupportedToolCallOutput(item)
			if !ok {
				changed = true
				continue
			}
			filtered = append(filtered, rewritten)
			changed = true
		default:
			filtered = append(filtered, json.RawMessage(item.Raw))
		}
	}
	if !changed {
		return body, nil
	}
	encoded, err := json.Marshal(filtered)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "input", encoded)
}

func rewriteGrokUnsupportedToolCallInput(item gjson.Result, itemType string) (json.RawMessage, bool) {
	name := strings.TrimSpace(item.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(item.Get("action").String())
	}
	if name == "" && itemType == "local_shell_call" {
		name = "local_shell"
	}
	if name == "" {
		return nil, false
	}

	callID := firstNonEmpty(
		strings.TrimSpace(item.Get("call_id").String()),
		strings.TrimSpace(item.Get("id").String()),
	)
	arguments := strings.TrimSpace(item.Get("arguments").String())
	if arguments == "" {
		if raw := item.Get("input"); raw.Exists() {
			arguments = grokToolCallArgumentsFromFreeform(raw)
		} else if raw := item.Get("action"); raw.IsObject() {
			if encoded, err := json.Marshal(raw.Value()); err == nil {
				arguments = string(encoded)
			}
		}
	}
	if arguments == "" {
		arguments = "{}"
	}

	out := map[string]any{
		"type":      "function_call",
		"name":      name,
		"arguments": arguments,
	}
	if callID != "" {
		out["call_id"] = callID
	}
	if status := strings.TrimSpace(item.Get("status").String()); status != "" {
		out["status"] = status
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(encoded), true
}

func rewriteGrokUnsupportedToolCallOutput(item gjson.Result) (json.RawMessage, bool) {
	callID := firstNonEmpty(
		strings.TrimSpace(item.Get("call_id").String()),
		strings.TrimSpace(item.Get("id").String()),
	)
	if callID == "" {
		return nil, false
	}
	output := item.Get("output")
	var outputValue any
	switch {
	case !output.Exists():
		outputValue = ""
	case output.Type == gjson.String:
		outputValue = output.String()
	default:
		outputValue = output.Value()
	}
	out := map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  outputValue,
	}
	if status := strings.TrimSpace(item.Get("status").String()); status != "" {
		out["status"] = status
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(encoded), true
}

func grokToolCallArgumentsFromFreeform(raw gjson.Result) string {
	switch raw.Type {
	case gjson.String:
		text := raw.String()
		trimmed := strings.TrimSpace(text)
		if trimmed == "" {
			return "{}"
		}
		// Already a JSON object/array/string literal — keep as function arguments.
		if (strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}")) ||
			(strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]")) ||
			(strings.HasPrefix(trimmed, `"`) && strings.HasSuffix(trimmed, `"`)) {
			if json.Valid([]byte(trimmed)) {
				return trimmed
			}
		}
		encoded, err := json.Marshal(map[string]string{"input": text})
		if err != nil {
			return "{}"
		}
		return string(encoded)
	case gjson.JSON:
		if raw.IsObject() || raw.IsArray() {
			return raw.Raw
		}
		fallthrough
	default:
		encoded, err := json.Marshal(map[string]any{"input": raw.Value()})
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
}

var grokResponsesSupportedToolTypes = map[string]struct{}{
	"code_execution":     {},
	"code_interpreter":   {},
	"collections_search": {},
	"file_search":        {},
	"function":           {},
	"mcp":                {},
	"shell":              {},
	"web_search":         {},
	"x_search":           {},
}

func sanitizeGrokResponsesTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return body, nil
	}

	rawTools := tools.Array()
	filteredTools := make([]json.RawMessage, 0, len(rawTools))
	seenNames := make(map[string]struct{}, len(rawTools))
	toolsChanged := false
	for _, tool := range rawTools {
		toolType := strings.TrimSpace(tool.Get("type").String())
		rawTool := json.RawMessage(tool.Raw)

		// Codex freeform/custom tools are not in xAI's tool enum; rewrite as
		// plain function tools so shell/apply_patch/etc stay callable.
		if toolType == "custom" || toolType == "freeform" {
			rewritten, ok := rewriteGrokCustomToolDefinition(tool)
			if !ok {
				toolsChanged = true
				continue
			}
			rawTool = rewritten
			tool = gjson.ParseBytes(rewritten)
			toolType = "function"
			toolsChanged = true
		}

		if _, ok := grokResponsesSupportedToolTypes[toolType]; !ok {
			toolsChanged = true
			continue
		}
		effectiveName := grokResponsesToolEffectiveName(tool)
		if toolType == "function" && isGrokNativeSearchToolName(effectiveName) {
			rawTool = json.RawMessage(`{"type":"` + effectiveName + `"}`)
			toolsChanged = true
		}
		if effectiveName != "" {
			if _, duplicate := seenNames[effectiveName]; duplicate {
				toolsChanged = true
				continue
			}
			seenNames[effectiveName] = struct{}{}
		}
		filteredTools = append(filteredTools, rawTool)
	}

	var err error
	if toolsChanged || len(filteredTools) != len(rawTools) {
		if len(filteredTools) == 0 {
			body, err = sjson.DeleteBytes(body, "tools")
		} else {
			var encoded []byte
			encoded, err = json.Marshal(filteredTools)
			if err != nil {
				return nil, err
			}
			body, err = sjson.SetRawBytes(body, "tools", encoded)
		}
		if err != nil {
			return nil, err
		}
	}

	toolChoice := gjson.GetBytes(body, "tool_choice")
	if !toolChoice.Exists() {
		return body, nil
	}
	if shouldDropGrokToolChoice(toolChoice, filteredTools) {
		body, err = sjson.DeleteBytes(body, "tool_choice")
		if err != nil {
			return nil, err
		}
	}
	return body, nil
}

func rewriteGrokCustomToolDefinition(tool gjson.Result) (json.RawMessage, bool) {
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(tool.Get("function.name").String())
	}
	if name == "" {
		return nil, false
	}
	description := strings.TrimSpace(tool.Get("description").String())
	if description == "" {
		description = strings.TrimSpace(tool.Get("function.description").String())
	}

	// Prefer an explicit JSON schema when Codex already provided one.
	parameters := tool.Get("parameters")
	if !parameters.Exists() {
		parameters = tool.Get("function.parameters")
	}
	var params any
	if parameters.Exists() && parameters.IsObject() {
		params = parameters.Value()
	} else {
		// Freeform custom tools often only carry format=text. Wrap the freeform
		// payload as a single string argument so Grok can still invoke the tool.
		params = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"input": map[string]any{
					"type":        "string",
					"description": "Freeform tool input",
				},
			},
			"required":             []string{"input"},
			"additionalProperties": true,
		}
	}

	out := map[string]any{
		"type":        "function",
		"name":        name,
		"parameters":   params,
		"strict":      false,
	}
	if description != "" {
		out["description"] = description
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return json.RawMessage(encoded), true
}

func isGrokNativeSearchToolName(name string) bool {
	return name == "web_search" || name == "x_search"
}

func grokResponsesToolEffectiveName(tool gjson.Result) string {
	toolType := strings.TrimSpace(tool.Get("type").String())
	name := strings.TrimSpace(tool.Get("name").String())
	if name == "" {
		name = strings.TrimSpace(tool.Get("function.name").String())
	}
	if name != "" {
		return name
	}
	switch toolType {
	case "web_search", "x_search":
		return toolType
	default:
		return ""
	}
}

func shouldDropGrokToolChoice(toolChoice gjson.Result, tools []json.RawMessage) bool {
	if len(tools) == 0 {
		return true
	}
	if !toolChoice.IsObject() {
		return false
	}
	choiceType := strings.TrimSpace(toolChoice.Get("type").String())
	if choiceType == "" {
		return false
	}
	if _, ok := grokResponsesSupportedToolTypes[choiceType]; !ok {
		return true
	}
	if choiceType == "function" {
		choiceName := strings.TrimSpace(toolChoice.Get("name").String())
		if choiceName == "" {
			choiceName = strings.TrimSpace(toolChoice.Get("function.name").String())
		}
		if choiceName == "" {
			return false
		}
		for _, tool := range tools {
			var item struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tool, &item); err != nil {
				continue
			}
			name := strings.TrimSpace(item.Name)
			if name == "" {
				name = strings.TrimSpace(item.Function.Name)
			}
			if strings.TrimSpace(item.Type) == "function" && name == choiceName {
				return false
			}
		}
		return true
	}
	return false
}

func (s *OpenAIGatewayService) bridgeGrokComposerImageInputs(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	token string,
) ([]byte, OpenAIUsage, bool, error) {
	if !shouldBridgeGrokComposerImageInputs(body) {
		return body, OpenAIUsage{}, false, nil
	}

	var reqBody map[string]any
	if err := json.Unmarshal(body, &reqBody); err != nil {
		return body, OpenAIUsage{}, false, fmt.Errorf("parse grok composer image bridge request: %w", err)
	}

	imageURLs := collectGrokComposerImageURLs(reqBody)
	if len(imageURLs) == 0 {
		return body, OpenAIUsage{}, false, nil
	}

	descriptions := make([]string, 0, len(imageURLs))
	var bridgeUsage OpenAIUsage
	for index, imageURL := range imageURLs {
		description, usage, err := s.describeGrokComposerImage(ctx, c, account, token, imageURL, index+1)
		if err != nil {
			return body, bridgeUsage, false, err
		}
		descriptions = append(descriptions, description)
		addOpenAIUsage(&bridgeUsage, usage)
	}

	if !rewriteGrokComposerImagesAsText(reqBody, descriptions) {
		return body, bridgeUsage, false, nil
	}
	bridgedBody, err := marshalOpenAIUpstreamJSON(reqBody)
	if err != nil {
		return body, bridgeUsage, false, fmt.Errorf("serialize grok composer image bridge request: %w", err)
	}
	return bridgedBody, bridgeUsage, true, nil
}

func shouldBridgeGrokComposerImageInputs(body []byte) bool {
	if len(body) == 0 || !isGrokComposerModel(gjson.GetBytes(body, "model").String()) {
		return false
	}
	messages := gjson.GetBytes(body, "messages")
	if !messages.Exists() {
		return false
	}
	return openAIJSONValueMayContainImageInput(messages)
}

func isGrokComposerModel(model string) bool {
	model = strings.TrimSpace(strings.ToLower(model))
	if model == "" {
		return false
	}
	if strings.Contains(model, "/") {
		parts := strings.Split(model, "/")
		model = strings.TrimSpace(parts[len(parts)-1])
	}
	return strings.Contains(model, "composer")
}

func collectGrokComposerImageURLs(reqBody map[string]any) []string {
	messages, ok := reqBody["messages"].([]any)
	if !ok {
		return nil
	}

	var imageURLs []string
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range parts {
			if imageURL := grokComposerImageURLFromPart(part); imageURL != "" {
				imageURLs = append(imageURLs, imageURL)
			}
		}
	}
	return imageURLs
}

func grokComposerImageURLFromPart(part any) string {
	partMap, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	if strings.TrimSpace(strings.ToLower(fmt.Sprint(partMap["type"]))) != "image_url" {
		return ""
	}
	switch imageURL := partMap["image_url"].(type) {
	case string:
		return normalizeGrokComposerImageURL(imageURL)
	case map[string]any:
		raw, _ := imageURL["url"].(string)
		return normalizeGrokComposerImageURL(raw)
	default:
		return ""
	}
}

func normalizeGrokComposerImageURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || isEmptyBase64DataURI(trimmed) {
		return ""
	}
	return trimmed
}

func (s *OpenAIGatewayService) describeGrokComposerImage(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	token string,
	imageURL string,
	index int,
) (string, OpenAIUsage, error) {
	body, err := buildGrokComposerImageDescriptionBody(imageURL, index)
	if err != nil {
		return "", OpenAIUsage{}, err
	}

	upstreamCtx, releaseUpstreamCtx := detachUpstreamContext(ctx)
	// Image-description probes are auxiliary requests, not conversation turns.
	// Do not bind them to the caller's Grok prompt-cache identity.
	upstreamReq, err := buildGrokResponsesRequest(upstreamCtx, c, account, body, token, "")
	releaseUpstreamCtx()
	if err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("build grok composer image bridge request: %w", err)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return "", OpenAIUsage{}, s.handleOpenAIUpstreamTransportError(ctx, c, account, err, false)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		respBody := s.readUpstreamErrorBody(resp)
		upstreamMsg := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(respBody))
		if upstreamMsg == "" {
			upstreamMsg = fmt.Sprintf("xAI image bridge upstream returned status %d", resp.StatusCode)
		}
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			UpstreamRequestID:  firstNonEmpty(resp.Header.Get("x-request-id"), resp.Header.Get("xai-request-id")),
			Kind:               "failover",
			Message:            upstreamMsg,
		})
		s.handleGrokAccountUpstreamError(ctx, account, resp.StatusCode, resp.Header, respBody)
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			return "", OpenAIUsage{}, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && account.IsPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return "", OpenAIUsage{}, fmt.Errorf("grok composer image bridge upstream error: %s", upstreamMsg)
	}

	s.updateGrokUsageSnapshot(ctx, account, xai.ParseQuotaHeaders(resp.Header, resp.StatusCode))
	respBody, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, nil)
	if err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("read grok composer image bridge response: %w", err)
	}

	var parsed apicompat.ResponsesResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return "", OpenAIUsage{}, fmt.Errorf("parse grok composer image bridge response: %w", err)
	}
	description := strings.TrimSpace(grokResponsesOutputText(&parsed))
	if description == "" {
		return "", copyOpenAIUsageFromResponsesUsage(parsed.Usage), fmt.Errorf("grok composer image bridge returned empty description")
	}
	return description, copyOpenAIUsageFromResponsesUsage(parsed.Usage), nil
}

func buildGrokComposerImageDescriptionBody(imageURL string, index int) ([]byte, error) {
	prompt := fmt.Sprintf("Describe image %d in concise, factual text for a downstream coding/composer model. Include visible text, UI elements, diagrams, errors, and spatial relationships. Do not mention that you are an image analysis bridge.", index)
	req := map[string]any{
		"model":             grokComposerImageBridgeVisionModel,
		"stream":            false,
		"store":             false,
		"max_output_tokens": grokComposerImageBridgeMaxOutputTokens,
		"input": []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": prompt},
					map[string]any{"type": "input_image", "image_url": imageURL},
				},
			},
		},
	}
	return marshalOpenAIUpstreamJSON(req)
}

func grokResponsesOutputText(resp *apicompat.ResponsesResponse) string {
	if resp == nil {
		return ""
	}
	var parts []string
	for _, output := range resp.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" || content.Type == "text" || content.Type == "input_text" {
				if text := strings.TrimSpace(content.Text); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

func rewriteGrokComposerImagesAsText(reqBody map[string]any, descriptions []string) bool {
	messages, ok := reqBody["messages"].([]any)
	if !ok {
		return false
	}

	imageIndex := 0
	changed := false
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msgMap["content"].([]any)
		if !ok {
			continue
		}
		var textParts []string
		messageChanged := false
		for _, part := range parts {
			if imageURL := grokComposerImageURLFromPart(part); imageURL != "" {
				if imageIndex < len(descriptions) {
					textParts = append(textParts, fmt.Sprintf("Image %d description: %s", imageIndex+1, strings.TrimSpace(descriptions[imageIndex])))
				}
				imageIndex++
				messageChanged = true
				continue
			}
			if text := grokComposerTextFromPart(part); text != "" {
				textParts = append(textParts, text)
			}
		}
		if messageChanged {
			msgMap["content"] = strings.Join(textParts, "\n\n")
			changed = true
		}
	}
	return changed
}

func grokComposerTextFromPart(part any) string {
	partMap, ok := part.(map[string]any)
	if !ok {
		return ""
	}
	partType := strings.TrimSpace(strings.ToLower(fmt.Sprint(partMap["type"])))
	switch partType {
	case "text", "input_text":
		text, _ := partMap["text"].(string)
		return strings.TrimSpace(text)
	default:
		return ""
	}
}

func addOpenAIUsage(dst *OpenAIUsage, usage OpenAIUsage) {
	if dst == nil {
		return
	}
	dst.InputTokens += usage.InputTokens
	dst.ImageInputTokens += usage.ImageInputTokens
	dst.OutputTokens += usage.OutputTokens
	dst.CacheCreationInputTokens += usage.CacheCreationInputTokens
	dst.CacheReadInputTokens += usage.CacheReadInputTokens
	dst.ImageOutputTokens += usage.ImageOutputTokens
}

func buildGrokResponsesRequest(ctx context.Context, c *gin.Context, account *Account, body []byte, token, cacheIdentity string) (*http.Request, error) {
	targetURL, err := xai.BuildResponsesURL(account.GetGrokBaseURL())
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	applyGrokCLIHeaders(req.Header)
	applyGrokChatProxyHeaders(req, account)
	applyGrokCacheHeaders(req.Header, cacheIdentity)
	if c != nil {
		if v := c.GetHeader("OpenAI-Beta"); strings.TrimSpace(v) != "" {
			req.Header.Set("OpenAI-Beta", v)
		}
	}
	return req, nil
}

func applyGrokChatProxyHeaders(req *http.Request, account *Account) {
	if req == nil || account == nil || !account.IsGrokOAuth() {
		return
	}
	baseURL := strings.TrimRight(strings.TrimSpace(account.GetGrokBaseURL()), "/")
	if !strings.EqualFold(baseURL, xai.DefaultCLIBaseURL) {
		return
	}
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-grok-client-version", grokCLIVersion)
	req.Header.Set("x-authenticateresponse", "authenticate-response")
}

// applyGrokCLIHeaders identifies subscription traffic as a supported Grok CLI
// version. The CLI gateway rejects otherwise valid OAuth requests without it.
func applyGrokCLIHeaders(headers http.Header) {
	if headers == nil {
		return
	}
	headers.Set("User-Agent", grokUpstreamUserAgent)
	headers.Set("X-Grok-Client-Version", grokCLIVersion)
}

func (s *OpenAIGatewayService) updateGrokUsageSnapshot(ctx context.Context, account *Account, snapshot *xai.QuotaSnapshot) {
	if s == nil || account == nil || account.ID <= 0 || snapshot == nil {
		return
	}
	accountID := account.ID
	now := time.Now()
	resetAt, hasActiveLimit := grokRateLimitResetAt(snapshot, now)
	if hasActiveLimit {
		normalizeGrokExhaustedWindowResets(snapshot, resetAt, now)
	}
	critical := snapshot.StatusCode == http.StatusTooManyRequests || hasActiveLimit
	if s.codexSnapshotThrottle != nil {
		allowed := s.codexSnapshotThrottle.Allow(accountID, now)
		if !critical && !allowed {
			return
		}
	}

	stateCtx := ctx
	if hasActiveLimit {
		var cancel context.CancelFunc
		stateCtx, cancel = openAIAccountStateContext(ctx)
		defer cancel()
	}
	if s.accountRepo != nil {
		_ = s.accountRepo.UpdateExtra(stateCtx, accountID, map[string]any{
			grokQuotaSnapshotExtraKey: snapshot,
		})
	}
	// Error responses are reconciled by handleGrokAccountUpstreamError, which
	// also installs the immediate in-memory scheduling block. Successful
	// responses can still consume the last available request/token, so persist
	// that exhausted window here as a real rate limit rather than relying only
	// on the passive snapshot scheduler check.
	if hasActiveLimit {
		s.rateLimitGrok(stateCtx, account, resetAt)
	}
}

func parseGrokQuotaSnapshot(headers http.Header, statusCode int, now time.Time) *xai.QuotaSnapshot {
	snapshot := xai.ParseQuotaHeaders(headers, statusCode)
	if snapshot == nil && statusCode == http.StatusTooManyRequests {
		return &xai.QuotaSnapshot{
			StatusCode: statusCode,
			UpdatedAt:  now.UTC().Format(time.RFC3339),
		}
	}
	return snapshot
}

// grokFreeUsageExhaustedInfo captures the free-tier quota-exhaustion 429 that
// xAI returns without Retry-After or ratelimit reset headers.
type grokFreeUsageExhaustedInfo struct {
	Model       string
	TokensUsed  *int64
	TokensLimit *int64
}

func isGrokFreeUsageExhaustedSnapshot(snapshot *xai.QuotaSnapshot) bool {
	if snapshot == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(snapshot.ObservationSource), grokFreeUsageExhaustedSource) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(snapshot.EntitlementStatus), grokFreeUsageExhaustedStatus)
}

func parseGrokFreeUsageExhausted(body []byte) *grokFreeUsageExhaustedInfo {
	if len(body) == 0 {
		return nil
	}
	code := strings.TrimSpace(gjson.GetBytes(body, "code").String())
	errorText := strings.TrimSpace(gjson.GetBytes(body, "error").String())
	if errorText == "" {
		errorText = strings.TrimSpace(gjson.GetBytes(body, "error.message").String())
	}
	if errorText == "" {
		errorText = strings.TrimSpace(gjson.GetBytes(body, "message").String())
	}
	normalizedCode := strings.ToLower(code)
	normalizedText := strings.ToLower(errorText)
	rawLower := strings.ToLower(string(body))
	matched := normalizedCode == grokFreeUsageExhaustedCode ||
		strings.Contains(normalizedCode, "free-usage-exhausted") ||
		strings.Contains(normalizedText, "free-usage-exhausted") ||
		strings.Contains(normalizedText, "included free usage") ||
		strings.Contains(rawLower, "subscription:free-usage-exhausted")
	if !matched {
		return nil
	}

	info := &grokFreeUsageExhaustedInfo{}
	// "for model grok-4.5-build-free for now"
	if idx := strings.Index(normalizedText, "for model "); idx >= 0 {
		rest := strings.TrimSpace(errorText[idx+len("for model "):])
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			info.Model = strings.Trim(fields[0], ".,;:\"'")
		}
	}
	// "tokens (actual/limit): 2138145/2000000" (may be followed by punctuation)
	if idx := strings.Index(normalizedText, "tokens (actual/limit):"); idx >= 0 {
		rest := strings.TrimSpace(errorText[idx+len("tokens (actual/limit):"):])
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			ratio := strings.Trim(fields[0], ".,;:\"'")
			parts := strings.Split(ratio, "/")
			if len(parts) == 2 {
				if used, err := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64); err == nil {
					info.TokensUsed = &used
				}
				if limit, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err == nil {
					info.TokensLimit = &limit
				}
			}
		}
	}
	return info
}

// applyGrokFreeUsageExhaustedToSnapshot synthesizes a durable token window reset
// when free-tier 429 responses omit Retry-After / x-ratelimit-reset headers.
// Without this, the account would only cool down for grokRateLimitFallbackCooldown
// and keep being reselected for ~24h of guaranteed failures.
func applyGrokFreeUsageExhaustedToSnapshot(snapshot *xai.QuotaSnapshot, info *grokFreeUsageExhaustedInfo, now time.Time) *xai.QuotaSnapshot {
	if info == nil {
		return snapshot
	}
	if snapshot == nil {
		snapshot = &xai.QuotaSnapshot{
			StatusCode: http.StatusTooManyRequests,
			UpdatedAt:  now.UTC().Format(time.RFC3339),
		}
	}
	if snapshot.StatusCode == 0 {
		snapshot.StatusCode = http.StatusTooManyRequests
	}
	if strings.TrimSpace(snapshot.UpdatedAt) == "" {
		snapshot.UpdatedAt = now.UTC().Format(time.RFC3339)
	}
	snapshot.ObservationSource = grokFreeUsageExhaustedSource
	if strings.TrimSpace(snapshot.EntitlementStatus) == "" {
		snapshot.EntitlementStatus = grokFreeUsageExhaustedStatus
	}
	if snapshot.Headers == nil {
		snapshot.Headers = make(map[string]string)
	}
	snapshot.Headers["x-sub2api-grok-error-code"] = grokFreeUsageExhaustedCode
	if info.Model != "" {
		snapshot.Headers["x-sub2api-grok-free-model"] = info.Model
	}

	resetAt := now.Add(grokFreeUsageExhaustedCooldown)
	resetUnix := resetAt.Unix()
	remaining := int64(0)
	window := snapshot.Tokens
	if window == nil {
		window = &xai.QuotaWindow{}
		snapshot.Tokens = window
	}
	if window.Remaining == nil || *window.Remaining > 0 {
		window.Remaining = &remaining
	}
	if window.Limit == nil {
		switch {
		case info.TokensLimit != nil && *info.TokensLimit > 0:
			window.Limit = info.TokensLimit
		case info.TokensUsed != nil && *info.TokensUsed > 0:
			// Upstream sometimes only reports used tokens; keep a positive limit so
			// quota UI can render remaining=0% with the synthetic reset time.
			window.Limit = info.TokensUsed
		default:
			one := int64(1)
			window.Limit = &one
		}
	}
	// Prefer an already-known future reset from headers; otherwise use the
	// rolling 24h free-usage window described by xAI.
	hasFutureReset := false
	if window.ResetUnix != nil && *window.ResetUnix > now.Unix() {
		hasFutureReset = true
	} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil && parsed.After(now) {
		hasFutureReset = true
	}
	if !hasFutureReset {
		window.ResetUnix = &resetUnix
		window.ResetAt = resetAt.UTC().Format(time.RFC3339)
	}
	snapshot.HeadersObserved = true
	if strings.TrimSpace(snapshot.LastHeadersSeenAt) == "" {
		snapshot.LastHeadersSeenAt = now.UTC().Format(time.RFC3339)
	}
	return snapshot
}

func normalizeGrokExhaustedWindowResets(snapshot *xai.QuotaSnapshot, resetAt, now time.Time) {
	if snapshot == nil || !resetAt.After(now) {
		return
	}
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if !candidate.After(now) {
			candidate = resetAt
		}
		resetUnix := candidate.Unix()
		window.ResetUnix = &resetUnix
		window.ResetAt = candidate.UTC().Format(time.RFC3339)
	}
}

func grokRateLimitResetAt(snapshot *xai.QuotaSnapshot, now time.Time) (time.Time, bool) {
	if snapshot == nil {
		return time.Time{}, false
	}

	// Retry-After is xAI's explicit retry boundary. Use the observation time so
	// a persisted snapshot does not start a fresh cooldown every time it is read.
	retryAfterExpired := false
	var resetAt time.Time
	if snapshot.RetryAfterSeconds != nil && *snapshot.RetryAfterSeconds > 0 {
		observedAt := now
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(snapshot.UpdatedAt)); err == nil {
			observedAt = parsed
		}
		retryAfterResetAt := observedAt.Add(time.Duration(*snapshot.RetryAfterSeconds) * time.Second)
		if retryAfterResetAt.After(now) {
			resetAt = retryAfterResetAt
		} else {
			retryAfterExpired = true
		}
	}

	exhausted := false
	for _, window := range []*xai.QuotaWindow{snapshot.Requests, snapshot.Tokens} {
		if window == nil || window.Remaining == nil || *window.Remaining > 0 {
			continue
		}
		exhausted = true
		candidate := time.Time{}
		if window.ResetUnix != nil && *window.ResetUnix > 0 {
			candidate = time.Unix(*window.ResetUnix, 0)
		} else if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(window.ResetAt)); err == nil {
			candidate = parsed
		}
		if candidate.After(now) && candidate.After(resetAt) {
			resetAt = candidate
		}
	}
	if !resetAt.IsZero() {
		return resetAt, true
	}
	// An observed Retry-After is an absolute boundary once combined with the
	// snapshot timestamp. Do not turn an expired persisted snapshot into a new
	// rolling fallback cooldown, but still allow a later explicit window reset.
	if retryAfterExpired {
		return time.Time{}, false
	}
	if exhausted || snapshot.StatusCode == http.StatusTooManyRequests {
		if isGrokFreeUsageExhaustedSnapshot(snapshot) {
			return now.Add(grokFreeUsageExhaustedCooldown), true
		}
		return now.Add(grokRateLimitFallbackCooldown), true
	}
	return time.Time{}, false
}

func normalizeGrokRateLimitResetAt(account *Account, resetAt, now time.Time) time.Time {
	if !resetAt.After(now) {
		resetAt = now.Add(grokRateLimitFallbackCooldown)
	}
	if account != nil && account.RateLimitResetAt != nil && account.RateLimitResetAt.After(resetAt) {
		resetAt = *account.RateLimitResetAt
	}
	return resetAt
}

type grokRateLimitExtendingRepository interface {
	SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error
}

func persistGrokRateLimit(ctx context.Context, repo AccountRepository, account *Account, resetAt time.Time) {
	if repo == nil || account == nil || account.ID <= 0 {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())
	stateCtx, cancel := openAIAccountStateContext(ctx)
	defer cancel()
	var err error
	if extendingRepo, ok := repo.(grokRateLimitExtendingRepository); ok {
		err = extendingRepo.SetRateLimitedIfLater(stateCtx, account.ID, resetAt)
	} else {
		err = repo.SetRateLimited(stateCtx, account.ID, resetAt)
	}
	if err != nil {
		slog.Warn("persist_grok_rate_limit_failed", "account_id", account.ID, "reset_at", resetAt.UTC(), "error", err)
	}
}

func (s *OpenAIGatewayService) rateLimitGrok(ctx context.Context, account *Account, resetAt time.Time) {
	if s == nil || account == nil {
		return
	}
	resetAt = normalizeGrokRateLimitResetAt(account, resetAt, time.Now())

	runtimeUntil := resetAt
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(runtimeUntil) {
		runtimeUntil = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, runtimeUntil, "429")
	persistGrokRateLimit(ctx, s.accountRepo, account, resetAt)
}

func (s *OpenAIGatewayService) handleGrokAccountUpstreamError(ctx context.Context, account *Account, statusCode int, headers http.Header, responseBody []byte) {
	if s == nil || account == nil {
		return
	}
	switch statusCode {
	case http.StatusUnauthorized:
		s.tempUnscheduleGrok(ctx, account, 10*time.Minute, "grok credentials unauthorized")
	case http.StatusForbidden:
		s.tempUnscheduleGrok(ctx, account, 30*time.Minute, "grok access or entitlement denied")
	case http.StatusTooManyRequests:
		now := time.Now()
		snapshot := parseGrokQuotaSnapshot(headers, statusCode, now)
		if info := parseGrokFreeUsageExhausted(responseBody); info != nil {
			snapshot = applyGrokFreeUsageExhaustedToSnapshot(snapshot, info, now)
		}
		s.updateGrokUsageSnapshot(ctx, account, snapshot)
		// updateGrokUsageSnapshot installs both runtime and durable rate-limit state.
	default:
		if statusCode >= 500 {
			s.tempUnscheduleGrok(ctx, account, 2*time.Minute, "grok upstream temporary error")
		}
	}
}

func (s *OpenAIGatewayService) tempUnscheduleGrok(ctx context.Context, account *Account, cooldown time.Duration, reason string) {
	if s == nil || account == nil {
		return
	}
	until := time.Now().Add(cooldown)
	if account.TempUnschedulableUntil != nil && account.TempUnschedulableUntil.After(until) {
		until = *account.TempUnschedulableUntil
	}
	s.BlockAccountScheduling(account, until, reason)
	if s.accountRepo != nil {
		stateCtx, cancel := openAIAccountStateContext(ctx)
		defer cancel()
		_ = s.accountRepo.SetTempUnschedulable(stateCtx, account.ID, until, reason)
	}
}
