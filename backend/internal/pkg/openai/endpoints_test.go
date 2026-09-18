package openai

import "testing"

func TestOAuthEndpointsRewrite(t *testing.T) {
	t.Parallel()
	e := OAuthEndpoints{
		ResponsesURL:    "http://codex.example/relay/responses",
		ChatGPTBaseURL:  "http://chat.example/prefix",
		AuthBaseURL:     "http://auth.example/login",
		PlatformBaseURL: "http://api.example/platform",
	}
	for _, tc := range []struct{ name, from, want string }{
		{"responses", DefaultResponsesURL, "http://codex.example/relay/responses"},
		{"compact", DefaultResponsesURL + "/compact", "http://codex.example/relay/responses/compact"},
		{"models", DefaultChatGPTBaseURL + "/backend-api/codex/models?client_version=1", "http://codex.example/relay/models?client_version=1"},
		{"images", DefaultChatGPTBaseURL + "/backend-api/codex/images/generations", "http://codex.example/relay/images/generations"},
		{"search", DefaultChatGPTBaseURL + "/backend-api/codex/alpha/search", "http://codex.example/relay/alpha/search"},
		{"live", "wss://chatgpt.com/backend-api/codex/call%2Fid", "ws://codex.example/relay/call%2Fid"},
		{"usage", DefaultChatGPTBaseURL + "/backend-api/wham/usage", "http://chat.example/prefix/backend-api/wham/usage"},
		{"privacy", DefaultChatGPTBaseURL + "/backend-api/settings/account_user_setting", "http://chat.example/prefix/backend-api/settings/account_user_setting"},
		{"subscription", DefaultChatGPTBaseURL + "/backend-api/subscriptions", "http://chat.example/prefix/backend-api/subscriptions"},
		{"file", DefaultChatGPTBaseURL + "/backend-api/files/file-1/download", "http://chat.example/prefix/backend-api/files/file-1/download"},
		{"authorize", AuthorizeURL + "?state=abc&redirect_uri=http%3A%2F%2Flocalhost", "http://auth.example/login/oauth/authorize?state=abc&redirect_uri=http%3A%2F%2Flocalhost"},
		{"refresh", TokenURL, "http://auth.example/login/oauth/token"},
		{"count tokens", DefaultPlatformBaseURL + "/v1/responses/input_tokens", "http://api.example/platform/v1/responses/input_tokens"},
		{"external signed download", "https://files.example/image?sig=123", "https://files.example/image?sig=123"},
		{"unrelated host", "https://chatgpt.com.example/backend-api/wham/usage", "https://chatgpt.com.example/backend-api/wham/usage"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.Rewrite(tc.from); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			if got := (OAuthEndpoints{}).Rewrite(tc.from); got != tc.from {
				t.Fatalf("defaults changed URL: %q", got)
			}
		})
	}
}

func TestOAuthEndpointsChatGPTBaseFallback(t *testing.T) {
	t.Parallel()
	for _, responses := range []string{"", DefaultResponsesURL} {
		e := OAuthEndpoints{ResponsesURL: responses, ChatGPTBaseURL: "http://relay.example/prefix/"}
		if got := e.Rewrite(DefaultResponsesURL); got != "http://relay.example/prefix/backend-api/codex/responses" {
			t.Fatal(got)
		}
	}
}

func TestOAuthEndpointsCustomResponsesPath(t *testing.T) {
	t.Parallel()
	e := OAuthEndpoints{ResponsesURL: "http://relay.example/custom/generate"}
	if got := e.Rewrite(DefaultResponsesURL); got != "http://relay.example/custom/generate" {
		t.Fatal(got)
	}
	if got := e.Rewrite(DefaultResponsesURL + "/compact"); got != "http://relay.example/custom/generate/compact" {
		t.Fatal(got)
	}
	if got := e.Rewrite(DefaultChatGPTBaseURL + "/backend-api/codex/models"); got != "http://relay.example/custom/models" {
		t.Fatal(got)
	}
}

func TestOAuthEndpointsRejectInvalidConfiguration(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"ftp://relay.example", "relay.example", "http://user:password@relay.example", "https://relay.example?key=secret", "https://relay.example#fragment", "http:///missing-host"} {
		t.Run(raw, func(t *testing.T) {
			e := OAuthEndpoints{AuthBaseURL: raw}
			if e.Validate() == nil {
				t.Fatal("invalid endpoint accepted")
			}
			if got := e.Rewrite(TokenURL); got == TokenURL {
				t.Fatal("invalid setting fell back to official host")
			}
		})
	}
	if _, err := OAuthEndpointsFromCredentials(map[string]any{"oauth_endpoints": "bad"}); err == nil {
		t.Fatal("invalid object accepted")
	}
	if _, err := OAuthEndpointsFromCredentials(map[string]any{"oauth_endpoints": map[string]any{"auth_base_url": 123}}); err == nil {
		t.Fatal("invalid field accepted")
	}
}
