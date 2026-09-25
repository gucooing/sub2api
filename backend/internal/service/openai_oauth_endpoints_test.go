package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/gin-gonic/gin"
	"github.com/imroc/req/v3"
	"github.com/stretchr/testify/require"
)

func TestOpenAIOAuthEndpointsRequests(t *testing.T) {
	parent := Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{
		"chatgpt_account_id": "account-id",
		"oauth_endpoints":    openai.OAuthEndpoints{ResponsesURL: "http://relay.example/custom/responses", PlatformBaseURL: "http://platform.example"},
	}}
	parentID := parent.ID
	shadow := Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID}
	svc := &OpenAIGatewayService{accountRepo: &stubOpenAIAccountRepo{accounts: []Account{parent}}}
	for _, account := range []*Account{&parent, &shadow} {
		t.Run(account.Type+strconv.FormatInt(account.ID, 10), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
			for _, passthrough := range []bool{false, true} {
				var request *http.Request
				var err error
				if passthrough {
					request, err = svc.buildUpstreamRequestOpenAIPassthrough(c.Request.Context(), c, account, []byte(`{"model":"gpt-5"}`), "test-token")
				} else {
					request, err = svc.buildUpstreamRequest(c.Request.Context(), c, account, []byte(`{"model":"gpt-5"}`), "test-token", false, "", false)
				}
				if err != nil {
					t.Fatal(err)
				}
				if request.URL.String() != "http://relay.example/custom/responses/compact" || request.Host != "relay.example" {
					t.Fatalf("wrong destination: %s Host=%s", request.URL, request.Host)
				}
				if request.Header.Get("Authorization") != "Bearer test-token" {
					t.Fatal("missing OAuth authentication")
				}
			}
			wsURL, err := svc.buildOpenAIResponsesWSURL(c.Request.Context(), account)
			if err != nil || wsURL != "ws://relay.example/custom/responses" {
				t.Fatalf("WS URL=%s err=%v", wsURL, err)
			}
			countRequest, err := svc.buildInputTokensUpstreamRequest(c.Request.Context(), c, account, []byte(`{}`), "test-token")
			if err != nil || countRequest.URL.String() != "http://platform.example/v1/responses/input_tokens" {
				t.Fatalf("count request=%v err=%v", countRequest, err)
			}
		})
	}
}

func TestOpenAIOAuthEndpointsAuthorizationSession(t *testing.T) {
	client := &openaiOAuthClientStateStub{}
	svc := NewOpenAIOAuthService(nil, client)
	defer svc.Stop()
	e := openai.OAuthEndpoints{AuthBaseURL: "http://auth.example/prefix", ChatGPTBaseURL: "http://chat.example"}
	result, err := svc.GenerateAuthURL(context.Background(), nil, "", PlatformOpenAI, e)
	if err != nil {
		t.Fatal(err)
	}
	authURL, err := url.Parse(result.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if authURL.Scheme != "http" || authURL.Host != "auth.example" || authURL.Path != "/prefix/oauth/authorize" {
		t.Fatal(result.AuthURL)
	}
	info, err := svc.ExchangeCode(context.Background(), &OpenAIExchangeCodeInput{SessionID: result.SessionID, Code: "test-code", State: authURL.Query().Get("state")})
	if err != nil {
		t.Fatal(err)
	}
	if info.OAuthEndpoints != e {
		t.Fatal("session endpoints lost")
	}
	saved, err := openai.OAuthEndpointsFromCredentials(svc.BuildAccountCredentials(info))
	if err != nil || saved != e {
		t.Fatalf("endpoints not persisted: %+v %v", saved, err)
	}
}

func TestOpenAIOAuthEndpointsHTTPPolicy(t *testing.T) {
	e := openai.OAuthEndpoints{AuthBaseURL: "http://relay.example/auth"}
	cfg := &config.Config{}
	if err := validateOpenAIOAuthEndpoints(e, cfg); err == nil {
		t.Fatal("disabled HTTP was accepted")
	}
	cfg.Security.URLAllowlist.AllowInsecureHTTP = true
	if err := validateOpenAIOAuthEndpoints(e, cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Security.URLAllowlist.Enabled = true
	cfg.Security.URLAllowlist.UpstreamHosts = []string{"allowed.example"}
	if err := validateOpenAIOAuthEndpoints(e, cfg); err == nil {
		t.Fatal("host allowlist was bypassed")
	}
}

func TestOpenAIOAuthEndpointsQuotaAndPrivacyHTTP(t *testing.T) {
	paths := make(chan string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths <- r.URL.Path
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing auth")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/relay/backend-api/wham/usage":
			_, _ = w.Write([]byte(`{"rate_limit_reset_credits":{"available_count":2},"credits":{"has_credits":true,"balance":"12.50"}}`))
		case "/relay/backend-api/wham/rate-limit-reset-credits":
			_, _ = w.Write([]byte(`{"available_count":2,"credits":[]}`))
		case "/relay/backend-api/wham/rate-limit-reset-credits/consume":
			_, _ = w.Write([]byte(`{"status":"success"}`))
		case "/relay/backend-api/settings/account_user_setting":
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	e := openai.OAuthEndpoints{ChatGPTBaseURL: srv.URL + "/relay"}
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth, Credentials: map[string]any{"chatgpt_account_id": "account-id", "oauth_endpoints": e}}
	parentID := account.ID
	shadow := &Account{ID: 2, Platform: PlatformOpenAI, Type: AccountTypeOAuth, ParentAccountID: &parentID}
	repo := &stubQuotaAccountRepo{accounts: map[int64]*Account{1: account, 2: shadow}}
	tokenCache := &stubQuotaTokenCache{tokens: map[string]string{OpenAITokenCacheKey(account): "test-token"}}
	factory := func(string) (*req.Client, error) { return req.C().SetTimeout(openaiQuotaUpstreamTimeout), nil }
	svc := NewOpenAIQuotaService(
		repo,
		nil,
		NewOpenAITokenProvider(repo, tokenCache, nil),
		factory,
		nil,
	)
	for _, id := range []int64{1, 2} {
		usage, err := svc.QueryUsage(context.Background(), id)
		require.NoError(t, err)
		require.NotNil(t, usage.RateLimitResetCredits)
		require.Equal(t, 2, usage.RateLimitResetCredits.AvailableCount)
		require.NotNil(t, usage.Credits)
		require.NotNil(t, usage.Credits.Balance)
		require.Equal(t, "12.50", *usage.Credits.Balance)
	}
	if _, err := svc.ResetCredit(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	if mode := disableOpenAITraining(context.Background(), factory, "test-token", "", e); mode != PrivacyModeTrainingOff {
		t.Fatal(mode)
	}
	if len(paths) != 6 {
		t.Fatalf("expected quota, reset and privacy requests, got %d", len(paths))
	}
	for len(paths) > 0 {
		if p := <-paths; len(p) < len("/relay/") || p[:len("/relay/")] != "/relay/" {
			t.Fatal(p)
		}
	}
}

func TestOpenAIOAuthQuotaEndpointDefaults(t *testing.T) {
	account := &Account{ID: 1, Platform: PlatformOpenAI, Type: AccountTypeOAuth}
	svc := &OpenAIQuotaService{accountRepo: &stubQuotaAccountRepo{accounts: map[int64]*Account{1: account}}}
	for _, officialURL := range []string{chatGPTUsageURL, chatGPTRateLimitCreditsURL, chatGPTRateLimitResetURL} {
		target, err := svc.oauthEndpoint(context.Background(), 1, officialURL)
		require.NoError(t, err)
		require.Equal(t, officialURL, target)
	}
}
