package openai

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

const (
	DefaultChatGPTBaseURL  = "https://chatgpt.com"
	DefaultAuthBaseURL     = "https://auth.openai.com"
	DefaultPlatformBaseURL = "https://api.openai.com"
	DefaultResponsesURL    = DefaultChatGPTBaseURL + "/backend-api/codex/responses"
)

// OAuthEndpoints describes account-specific upstreams. Empty fields use the
// official URLs. ResponsesURL also relocates sibling Codex endpoints (models,
// images, search and realtime); ChatGPTBaseURL handles the remaining backend APIs.
type OAuthEndpoints struct {
	PlatformBaseURL string `json:"platform_base_url,omitempty"`
	ResponsesURL    string `json:"responses_url,omitempty"`
	ChatGPTBaseURL  string `json:"chatgpt_base_url,omitempty"`
	AuthBaseURL     string `json:"auth_base_url,omitempty"`
}

func FirstOAuthEndpoints(values []OAuthEndpoints) OAuthEndpoints {
	if len(values) == 0 {
		return OAuthEndpoints{}
	}
	return values[0]
}

func (e OAuthEndpoints) IsZero() bool {
	return e == (OAuthEndpoints{})
}

func (e OAuthEndpoints) Values() map[string]string {
	return map[string]string{"responses_url": e.ResponsesURL, "chatgpt_base_url": e.ChatGPTBaseURL, "auth_base_url": e.AuthBaseURL, "platform_base_url": e.PlatformBaseURL}
}

func (e OAuthEndpoints) Validate() error {
	for key, raw := range e.Values() {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("oauth_endpoints.%s must be an absolute HTTP or HTTPS URL", key)
		}
		if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
			return fmt.Errorf("oauth_endpoints.%s cannot contain credentials, a query or a fragment", key)
		}
	}
	return nil
}

// Rewrite only relocates official OpenAI URLs. URLs returned by external file
// stores and URLs supplied by other providers retain their own destination.
func (e OAuthEndpoints) Rewrite(raw string) string {
	if e.IsZero() {
		return raw
	}
	if err := e.Validate(); err != nil {
		return "://invalid-openai-oauth-endpoint"
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	var base, suffix string
	switch strings.ToLower(u.Host) {
	case "chatgpt.com":
		codexPath := "/backend-api/codex"
		responses := strings.TrimRight(strings.TrimSpace(e.ResponsesURL), "/")
		if responses != "" && responses != DefaultResponsesURL && (u.Path == codexPath || strings.HasPrefix(u.Path, codexPath+"/")) {
			responsePath := codexPath + "/responses"
			if u.Path == responsePath || strings.HasPrefix(u.Path, responsePath+"/") {
				base = responses
				suffix = strings.TrimPrefix(u.EscapedPath(), responsePath)
			} else {
				parsed, _ := url.Parse(responses) // Validated above.
				base = parsed.Scheme + "://" + parsed.Host
				if parsed.Path != "" {
					base += strings.TrimRight(path.Dir(parsed.EscapedPath()), "/")
				}
				suffix = strings.TrimPrefix(u.EscapedPath(), codexPath)
			}
		} else {
			base = strings.TrimRight(strings.TrimSpace(e.ChatGPTBaseURL), "/")
			suffix = u.EscapedPath()
		}
	case "api.openai.com":
		base = strings.TrimRight(strings.TrimSpace(e.PlatformBaseURL), "/")
		suffix = u.EscapedPath()
	case "auth.openai.com":
		base = strings.TrimRight(strings.TrimSpace(e.AuthBaseURL), "/")
		suffix = u.EscapedPath()
	default:
		return raw
	}
	if base == "" {
		return raw
	}
	target := base + suffix
	if u.RawQuery != "" {
		target += "?" + u.RawQuery
	}
	if u.Scheme == "wss" || u.Scheme == "ws" {
		target = strings.Replace(target, "https://", "wss://", 1)
		target = strings.Replace(target, "http://", "ws://", 1)
	}
	return target
}

func OAuthEndpointsFromCredentials(credentials map[string]any) (OAuthEndpoints, error) {
	raw, ok := credentials["oauth_endpoints"]
	if !ok || raw == nil {
		return OAuthEndpoints{}, nil
	}
	if endpoints, ok := raw.(OAuthEndpoints); ok {
		return endpoints, endpoints.Validate()
	}
	values, ok := raw.(map[string]any)
	if !ok {
		return OAuthEndpoints{}, fmt.Errorf("oauth_endpoints must be an object")
	}
	var e OAuthEndpoints
	for key, dest := range map[string]*string{"responses_url": &e.ResponsesURL, "chatgpt_base_url": &e.ChatGPTBaseURL, "auth_base_url": &e.AuthBaseURL, "platform_base_url": &e.PlatformBaseURL} {
		if raw, exists := values[key]; exists {
			value, ok := raw.(string)
			if !ok {
				return e, fmt.Errorf("oauth_endpoints.%s must be a string", key)
			}
			*dest = strings.TrimSpace(value)
		}
	}
	return e, e.Validate()
}
