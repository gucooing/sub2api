package service

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	grokConversationIDHeader         = "X-Grok-Conv-Id"
	claudeCodeSessionHeader          = "X-Claude-Code-Session-Id"
	grokClientToolCacheOptInHeader   = "X-Sub2API-Grok-Client-Tool-Cache"
	grokFreeCacheNativeToolsJSON     = `[{"type":"web_search"},{"type":"x_search"}]`
	grokFreeCacheDisabledToolChoice  = "none"
	grokClientToolCacheOptInExtraKey = "grok_client_tool_cache_enabled"
)

// Claude Code metadata.user_id often ends with _session_<uuid>.
var claudeCodeSessionSuffixPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// extractClaudeCodeSessionID resolves the Claude Code conversation id from
// headers or Anthropic/OpenAI-compatible payload metadata.
func extractClaudeCodeSessionID(c *gin.Context, body []byte) string {
	if c != nil {
		if seed := strings.TrimSpace(c.GetHeader(claudeCodeSessionHeader)); seed != "" {
			return seed
		}
	}
	return extractClaudeCodeSessionIDFromPayload(body)
}

func extractClaudeCodeSessionIDFromPayload(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	userID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	if userID == "" {
		return ""
	}
	if matches := claudeCodeSessionSuffixPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return matches[1]
	}
	// Claude Code may embed JSON: {"session_id":"..."}
	if len(userID) > 0 && userID[0] == '{' {
		if sid := strings.TrimSpace(gjson.Get(userID, "session_id").String()); sid != "" {
			return sid
		}
	}
	return ""
}

// resolveGrokCacheIdentity derives one stable, tenant-isolated routing identity
// for xAI's server-side prompt cache. The returned value is safe to expose to
// the upstream: it never contains the client's raw session identifier.
//
// A valid downstream API key is required. This intentionally fails closed on
// internal probes and incomplete request contexts instead of creating a cache
// identity that could be shared by unrelated tenants.
func resolveGrokCacheIdentity(c *gin.Context, body []byte, explicitKey, upstreamModel string) string {
	apiKeyID := getAPIKeyIDFromContext(c)
	if apiKeyID <= 0 {
		return ""
	}
	// /responses/compact rejects tool_choice and does not represent a normal
	// conversation turn. Keep both cache identity and Free-tier routing
	// augmentation out of this path.
	if isOpenAIResponsesCompactPath(c) {
		return ""
	}

	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if model == "" {
		return ""
	}

	seed := explicitGrokCacheSeed(c, body, explicitKey)
	if seed == "" {
		seed = deriveOpenAIStablePrefixSessionSeed(body)
		if seed == "" {
			// A model alone is too broad for cache routing. Preserve the
			// existing first-user-derived identity when no reusable prefix is
			// available so unrelated prompts do not share one tenant-wide key.
			seed = deriveOpenAIAnchoredContentSessionSeed(body)
		}
	}
	if seed == "" {
		return ""
	}

	// generateSessionUUID hashes the whole seed before formatting it as a UUID.
	// Include a versioned namespace so this identity cannot collide with other
	// upstream session identifiers derived by sub2api.
	isolatedSeed := fmt.Sprintf("grok-prompt-cache:v1:%d:%s:%s", apiKeyID, model, seed)
	return generateSessionUUID(isolatedSeed)
}

func explicitGrokCacheSeed(c *gin.Context, body []byte, explicitKey string) string {
	// Claude Code session is the most stable multi-turn identity for
	// /v1/messages → Grok bridges. Prefer it over generic session headers so
	// prompt cache routing follows the gateway's existing cache affinity rules.
	seed := extractClaudeCodeSessionID(c, body)
	if seed == "" {
		seed = explicitOpenAIHeaderSessionID(c)
	}
	if seed == "" && c != nil {
		seed = strings.TrimSpace(c.GetHeader(grokConversationIDHeader))
	}
	if seed == "" && len(body) > 0 {
		seed = strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String())
	}
	if seed == "" {
		seed = strings.TrimSpace(explicitKey)
	}
	return seed
}

func isGrokRequestContext(c *gin.Context) bool {
	if c == nil {
		return false
	}
	if c.Request != nil {
		if platform, ok := ResolvedTargetPlatformFromContext(c.Request.Context()); ok {
			return platform == PlatformGrok
		}
	}
	v, exists := c.Get("api_key")
	if !exists {
		return false
	}
	apiKey, ok := v.(*APIKey)
	return ok && apiKey != nil && apiKey.Group != nil && apiKey.Group.Platform == PlatformGrok
}

// applyGrokResponsesCacheIdentity writes the cache routing identity into an
// xAI Responses request. Existing client values are deliberately replaced by
// the tenant-isolated value to prevent collisions on shared OAuth accounts.
//
// For OAuth tool-free requests, inject native web_search/x_search with
// tool_choice=none so xAI selects the cache-capable route without running a
// search. Explicit client tool intent (including tools later stripped by the
// sanitizer) is left alone here; Free accounts with client tools are handled by
// applyGrokFree* which always ensures missing web_search/x_search.
//
// Native-tool injection on the tool-free path is independent of cache identity:
// missing a session seed must not strand Free/OAuth on build-free.
func applyGrokResponsesCacheIdentity(body, intentSourceBody []byte, identity string, injectFreeTierTools bool) ([]byte, error) {
	identity = strings.TrimSpace(identity)
	out := body
	var err error
	if identity == "" {
		if gjson.GetBytes(body, "prompt_cache_key").Exists() {
			out, err = sjson.DeleteBytes(body, "prompt_cache_key")
			if err != nil {
				return nil, err
			}
		}
	} else {
		out, err = sjson.SetBytes(body, "prompt_cache_key", identity)
		if err != nil {
			return nil, err
		}
	}
	if !injectFreeTierTools {
		return out, nil
	}
	// Pre-sanitization intent wins: stripped unsupported tools must not be
	// rewritten into an eligible tool-free native-search request here.
	if hasGrokResponsesToolIntent(intentSourceBody) {
		return out, nil
	}
	out, err = ensureGrokNativeSearchTools(out)
	if err != nil {
		return nil, err
	}
	return sjson.SetBytes(out, "tool_choice", grokFreeCacheDisabledToolChoice)
}

func hasGrokResponsesToolIntent(body []byte) bool {
	// Empty/null tools arrays are not real tool intent — Free/OAuth still needs
	// native search injection on those requests. Only a non-empty tools list or
	// an explicit tool_choice counts.
	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() && len(tools.Array()) > 0 {
		return true
	}
	if gjson.GetBytes(body, "tool_choice").Exists() {
		return true
	}
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) != "additional_tools" {
			continue
		}
		carrierTools := item.Get("tools")
		if !carrierTools.Exists() || !carrierTools.IsArray() || len(carrierTools.Array()) > 0 {
			return true
		}
	}
	return false
}

// applyGrokFreeMessagesFunctionToolCacheRoute ensures Free OAuth requests carry
// native web_search/x_search whenever those tools are missing. Operators can
// explicitly disable injection per account (#4486).
func applyGrokFreeMessagesFunctionToolCacheRoute(body, intentSourceBody []byte, account *Account, cacheIdentity string) ([]byte, error) {
	return applyGrokFreeToolCacheRoute(body, intentSourceBody, account, cacheIdentity)
}

// applyGrokFreeRequestToolCacheRoute also accepts a request-scoped override via
// X-Sub2API-Grok-Client-Tool-Cache. The header is consumed locally because
// buildGrokResponsesRequest only forwards the supported OpenAI-Beta header.
// Explicit request opt-out always wins; request opt-in may override an account
// opt-out for a known Free account.
func applyGrokFreeRequestToolCacheRoute(c *gin.Context, body, intentSourceBody []byte, account *Account, cacheIdentity string) ([]byte, error) {
	enabled, _ := grokClientToolCacheAccountPolicy(account)
	if c != nil {
		switch strings.ToLower(strings.TrimSpace(c.GetHeader(grokClientToolCacheOptInHeader))) {
		case "1", "true", "yes", "on", "prefer-cache":
			// Request opt-in only makes sense for Free OAuth; paid stays fail-closed.
			if isKnownGrokFreeAccount(account) {
				enabled = true
			}
		case "0", "false", "no", "off":
			return body, nil
		}
	}
	if !enabled {
		return body, nil
	}
	_ = cacheIdentity
	_ = intentSourceBody
	return ensureGrokNativeSearchTools(body)
}

// grokClientToolCacheAccountPolicy reports whether Free native-tool injection is
// enabled for the account. Missing key defaults on for known Free OAuth; an
// explicit false disables injection. Paid/API-key/unknown remain disabled.
func grokClientToolCacheAccountPolicy(account *Account) (enabled, explicit bool) {
	if !isKnownGrokFreeAccount(account) {
		return false, false
	}
	if account.Extra == nil {
		return true, false
	}
	value, exists := account.Extra[grokClientToolCacheOptInExtraKey]
	if !exists {
		return true, false
	}
	enabled, valid := value.(bool)
	if !valid {
		return false, true
	}
	return enabled, true
}

func applyGrokFreeToolCacheRoute(body, intentSourceBody []byte, account *Account, cacheIdentity string) ([]byte, error) {
	// cacheIdentity is retained for call-site compatibility only. Free native
	// tool injection keys off account Free evidence and whether web_search /
	// x_search are missing — not session seeds or companion client tools.
	_ = cacheIdentity
	_ = intentSourceBody
	enabled, _ := grokClientToolCacheAccountPolicy(account)
	if !enabled {
		return body, nil
	}
	return ensureGrokNativeSearchTools(body)
}

func isKnownGrokFreeAccount(account *Account) bool {
	if account == nil || !account.IsGrokOAuth() {
		return false
	}
	freeSignal := false
	paidSignal := false
	inferredFreeSignal := false
	if billing, err := grokBillingSnapshotFromExtra(account.Extra); err == nil && billing != nil {
		if tier := strings.TrimSpace(billing.Plan); tier != "" {
			if isGrokFreeSubscriptionTier(tier) {
				freeSignal = true
			} else if !isGrokUnknownSubscriptionTier(tier) {
				paidSignal = true
			}
		}
		// Paid subscriptions expose a SuperGrok plan, monthly dollar limit, and/or
		// the weekly creditUsagePercent bar. Free OAuth deliberately omits those
		// fields (empty plan, no monthly limit, no weekly credit bar).
		if billing.UsagePercent != nil || billing.UsedPercent != nil ||
			(billing.MonthlyLimitCents != nil && *billing.MonthlyLimitCents > 0) {
			paidSignal = true
		}
		// xAI reports an empty plan for Free accounts. Any successful billing
		// window without paid markers is positive Free evidence — including the
		// common weekly-only partial probe where monthly returns an empty body
		// and BuildBillingSummary is nil. Pure dual-window failures stay
		// fail-closed so unobserved accounts do not opt into Free tool injection.
		if !paidSignal && strings.TrimSpace(billing.Plan) == "" &&
			grokBillingHasSuccessfulObservation(billing) {
			inferredFreeSignal = true
		}
	}
	if snapshot, err := grokQuotaSnapshotFromExtra(account.Extra); err == nil && snapshot != nil {
		if tier := strings.TrimSpace(snapshot.SubscriptionTier); tier != "" {
			if isGrokFreeSubscriptionTier(tier) {
				freeSignal = true
			} else if !isGrokUnknownSubscriptionTier(tier) {
				paidSignal = true
			}
		}
		if snapshot.Tokens != nil && snapshot.Tokens.Limit != nil &&
			xai.IsGrokFreeRolling24hTokenLimit(*snapshot.Tokens.Limit) {
			inferredFreeSignal = true
		}
	}
	if tier := strings.TrimSpace(account.GetCredential("subscription_tier")); tier != "" {
		if isGrokFreeSubscriptionTier(tier) {
			freeSignal = true
		} else if !isGrokUnknownSubscriptionTier(tier) {
			paidSignal = true
		}
	}
	// Explicit paid evidence always wins over an inferred Free signal. This
	// protects upgraded/stale accounts whose previous quota snapshot still
	// carries the historical 2M Free token limit.
	return !paidSignal && (freeSignal || inferredFreeSignal)
}

// grokBillingHasSuccessfulObservation reports whether billing extra contains at
// least one successfully refreshed window (or a fully successful dual probe).
// Dual-window transport/auth failures leave no Weekly/MonthlyUpdatedAt and must
// not be treated as Free evidence.
func grokBillingHasSuccessfulObservation(billing *xai.BillingSummary) bool {
	if billing == nil {
		return false
	}
	if strings.TrimSpace(billing.WeeklyUpdatedAt) != "" || strings.TrimSpace(billing.MonthlyUpdatedAt) != "" {
		return true
	}
	return billing.StatusCode >= http.StatusOK && billing.StatusCode < http.StatusMultipleChoices &&
		!billing.Partial && len(billing.FailedWindows) == 0
}

func isGrokFreeSubscriptionTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "free", "grok-free", "grok_free", "free-tier", "free_tier", "basic", "grok-basic", "grok_basic":
		return true
	default:
		return false
	}
}

func isGrokUnknownSubscriptionTier(tier string) bool {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "", "unknown", "n/a", "none":
		return true
	default:
		return false
	}
}

// ensureGrokNativeSearchTools makes sure web_search and x_search are present as
// native built-in tools. Missing entries are appended; function-declared search
// tools are promoted to native. Client tools other than search are preserved.
// An empty or absent tools array becomes just the two native search tools.
func ensureGrokNativeSearchTools(body []byte) ([]byte, error) {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() || len(tools.Array()) == 0 {
		return sjson.SetRawBytes(body, "tools", []byte(grokFreeCacheNativeToolsJSON))
	}

	items := tools.Array()
	merged := make([]json.RawMessage, 0, len(items)+2)
	present := make(map[string]bool, 2)
	for _, tool := range items {
		toolType := strings.TrimSpace(tool.Get("type").String())
		switch toolType {
		case "function":
			name := strings.TrimSpace(tool.Get("name").String())
			if name == "" {
				name = strings.TrimSpace(tool.Get("function.name").String())
			}
			// Promote client-declared search function tools to native built-ins.
			if name == "web_search" || name == "x_search" {
				if present[name] {
					continue
				}
				raw, err := json.Marshal(map[string]string{"type": name})
				if err != nil {
					return nil, err
				}
				merged = append(merged, raw)
				present[name] = true
				continue
			}
			merged = append(merged, json.RawMessage(tool.Raw))
		case "web_search", "x_search":
			if present[toolType] {
				continue
			}
			merged = append(merged, json.RawMessage(tool.Raw))
			present[toolType] = true
		default:
			merged = append(merged, json.RawMessage(tool.Raw))
		}
	}
	for _, toolType := range []string{"web_search", "x_search"} {
		if present[toolType] {
			continue
		}
		raw, err := json.Marshal(map[string]string{"type": toolType})
		if err != nil {
			return nil, err
		}
		merged = append(merged, raw)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(body, "tools", encoded)
}

func appendMissingGrokFreeCacheNativeTools(body []byte) ([]byte, error) {
	return ensureGrokNativeSearchTools(body)
}


// applyGrokCacheHeaders applies the documented Chat Completions conversation
// routing header. The request is built from a fresh header map, so client
// supplied x-grok headers cannot override this server-derived value.
func applyGrokCacheHeaders(headers http.Header, identity string) {
	if headers == nil {
		return
	}
	identity = strings.TrimSpace(identity)
	if identity == "" {
		headers.Del(grokConversationIDHeader)
		return
	}
	headers.Set(grokConversationIDHeader, identity)
}

// stripGrokChatPromptCacheKey removes the Responses-only body field after it
// has been used as an identity seed. Chat Completions routes cache by header.
func stripGrokChatPromptCacheKey(body []byte) ([]byte, error) {
	if !gjson.GetBytes(body, "prompt_cache_key").Exists() {
		return body, nil
	}
	return sjson.DeleteBytes(body, "prompt_cache_key")
}
