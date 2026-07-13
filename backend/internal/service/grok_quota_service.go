package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
)

const (
	grokQuotaUpstreamTimeout = 20 * time.Second
	grokCLIClientVersion     = "0.2.93"
	grokCLITokenAuth         = "xai-grok-cli"
	grokCLIUserAgent         = "grok-shell/0.2.93 (windows; x86_64)"
)

type GrokMoneyValue struct {
	Val float64 `json:"val"`
}

type GrokUsagePeriod struct {
	Type  string `json:"type,omitempty"`
	Start string `json:"start,omitempty"`
	End   string `json:"end,omitempty"`
}

type GrokBillingConfig struct {
	CurrentPeriod      *GrokUsagePeriod `json:"currentPeriod,omitempty"`
	CreditUsagePercent *float64         `json:"creditUsagePercent,omitempty"`
	MonthlyLimit       *GrokMoneyValue  `json:"monthlyLimit,omitempty"`
	Used               *GrokMoneyValue  `json:"used,omitempty"`
	OnDemandCap        *GrokMoneyValue  `json:"onDemandCap,omitempty"`
	OnDemandUsed       *GrokMoneyValue  `json:"onDemandUsed,omitempty"`
	PrepaidBalance     *GrokMoneyValue  `json:"prepaidBalance,omitempty"`
	BillingPeriodStart string           `json:"billingPeriodStart,omitempty"`
	BillingPeriodEnd   string           `json:"billingPeriodEnd,omitempty"`
}

type grokBillingResponse struct {
	Config GrokBillingConfig `json:"config"`
}

type GrokQuotaProbeResult struct {
	Source          string             `json:"source"`
	Model           string             `json:"model,omitempty"`
	Snapshot        *xai.QuotaSnapshot `json:"snapshot,omitempty"`
	Credits         *GrokBillingConfig `json:"credits,omitempty"`
	Monthly         *GrokBillingConfig `json:"monthly,omitempty"`
	StatusCode      int                `json:"status_code,omitempty"`
	HeadersObserved bool               `json:"headers_observed"`
	ResetSupported  bool               `json:"reset_supported"`
	FetchedAt       int64              `json:"fetched_at"`
}

type GrokQuotaResetResult struct {
	Supported bool   `json:"supported"`
	Code      string `json:"code"`
	Message   string `json:"message"`
}

type GrokQuotaService struct {
	accountRepo   AccountRepository
	proxyRepo     ProxyRepository
	tokenProvider *GrokTokenProvider
	httpUpstream  HTTPUpstream
}

func NewGrokQuotaService(
	accountRepo AccountRepository,
	proxyRepo ProxyRepository,
	tokenProvider *GrokTokenProvider,
	httpUpstream HTTPUpstream,
) *GrokQuotaService {
	return &GrokQuotaService{
		accountRepo:   accountRepo,
		proxyRepo:     proxyRepo,
		tokenProvider: tokenProvider,
		httpUpstream:  httpUpstream,
	}
}

// ProbeUsage uses the same read-only billing requests as the Grok CLI and does
// not consume a conversation request from the account.
func (s *GrokQuotaService) ProbeUsage(ctx context.Context, accountID int64) (*GrokQuotaProbeResult, error) {
	account, token, proxyURL, err := s.prepareProbe(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if grokAccountUserID(account) == "" {
		userID, fetchErr := s.fetchUserID(ctx, account, token, proxyURL)
		if fetchErr != nil {
			return nil, fetchErr
		}
		if account.Credentials == nil {
			account.Credentials = make(map[string]any)
		}
		account.Credentials["user_id"] = userID
		_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{"grok_user_id": userID})
	}

	credits, status, err := s.fetchBilling(ctx, account, token, proxyURL, true)
	if err != nil {
		return nil, err
	}
	monthly, _, monthlyErr := s.fetchBilling(ctx, account, token, proxyURL, false)
	if monthlyErr != nil {
		monthly = nil
	}

	// Billing does not expose the request/token windows carried by conversation
	// response headers. Preserve the latest observed windows instead of turning
	// creditUsagePercent or a free account's period into a synthetic quota.
	snapshot := grokSnapshotAfterBillingProbe(account, status)
	_ = s.accountRepo.UpdateExtra(ctx, account.ID, map[string]any{
		grokQuotaSnapshotExtraKey: snapshot,
	})
	return &GrokQuotaProbeResult{
		Source:          "billing_api",
		Snapshot:        snapshot,
		Credits:         credits,
		Monthly:         monthly,
		StatusCode:      status,
		HeadersObserved: snapshot.HeadersObserved,
		ResetSupported:  false,
		FetchedAt:       time.Now().Unix(),
	}, nil
}

func (s *GrokQuotaService) fetchUserID(
	ctx context.Context,
	account *Account,
	token string,
	proxyURL string,
) (string, error) {
	callCtx, cancel := context.WithTimeout(ctx, grokQuotaUpstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, xai.DefaultCLIBaseURL+"/user", nil)
	if err != nil {
		return "", infraerrors.Newf(
			http.StatusInternalServerError,
			"GROK_USER_REQUEST_BUILD_FAILED",
			"failed to build user request: %v",
			err,
		)
	}
	setGrokCLICommonHeaders(req, token)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return "", infraerrors.Newf(http.StatusBadGateway, "GROK_USER_REQUEST_FAILED", "user request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", infraerrors.Newf(
			mapUpstreamStatus(resp.StatusCode),
			"GROK_USER_UPSTREAM_ERROR",
			"user request returned %d",
			resp.StatusCode,
		)
	}

	var payload struct {
		UserID string `json:"userId"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", infraerrors.Newf(http.StatusBadGateway, "GROK_USER_RESPONSE_INVALID", "invalid user response: %v", err)
	}
	payload.UserID = strings.TrimSpace(payload.UserID)
	if payload.UserID == "" {
		return "", infraerrors.New(
			http.StatusBadGateway,
			"GROK_USER_ID_MISSING",
			"Grok /user response did not include userId",
		)
	}
	return payload.UserID, nil
}

func (s *GrokQuotaService) fetchBilling(
	ctx context.Context,
	account *Account,
	token string,
	proxyURL string,
	credits bool,
) (*GrokBillingConfig, int, error) {
	target := xai.DefaultCLIBaseURL + "/billing"
	if credits {
		target += "?format=credits"
	}
	callCtx, cancel := context.WithTimeout(ctx, grokQuotaUpstreamTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, target, nil)
	if err != nil {
		return nil, 0, infraerrors.Newf(
			http.StatusInternalServerError,
			"GROK_QUOTA_REQUEST_BUILD_FAILED",
			"failed to build billing request: %v",
			err,
		)
	}
	setGrokCLIHeaders(req, account, token)
	resp, err := s.httpUpstream.Do(req, proxyURL, account.ID, maxInt(account.Concurrency, 1))
	if err != nil {
		return nil, 0, infraerrors.Newf(
			http.StatusBadGateway,
			"GROK_QUOTA_REQUEST_FAILED",
			"billing request failed: %v",
			err,
		)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 240))
		return nil, resp.StatusCode, infraerrors.Newf(
			mapUpstreamStatus(resp.StatusCode),
			"GROK_QUOTA_UPSTREAM_ERROR",
			"billing returned %d: %s",
			resp.StatusCode,
			truncate(strings.TrimSpace(string(body)), 240),
		)
	}

	var payload grokBillingResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return nil, resp.StatusCode, infraerrors.Newf(
			http.StatusBadGateway,
			"GROK_QUOTA_RESPONSE_INVALID",
			"invalid billing response: %v",
			err,
		)
	}
	return &payload.Config, resp.StatusCode, nil
}

func setGrokCLIHeaders(req *http.Request, account *Account, token string) {
	setGrokCLICommonHeaders(req, token)
	req.Header.Set("x-userid", grokAccountUserID(account))
}

func setGrokCLICommonHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-XAI-Token-Auth", grokCLITokenAuth)
	req.Header.Set("x-grok-client-version", grokCLIClientVersion)
	req.Header.Set("x-authenticateresponse", "authenticate-response")
	req.Header.Set("User-Agent", grokCLIUserAgent)
}

func grokAccountUserID(account *Account) string {
	if account == nil {
		return ""
	}
	for _, key := range []string{"user_id", "userId", "sub"} {
		if userID := credentialString(account.Credentials, key); userID != "" {
			return userID
		}
	}
	if userID, _ := account.Extra["grok_user_id"].(string); strings.TrimSpace(userID) != "" {
		return strings.TrimSpace(userID)
	}
	return ""
}

func credentialString(credentials map[string]any, key string) string {
	if credentials == nil {
		return ""
	}
	value, _ := credentials[key].(string)
	return strings.TrimSpace(value)
}

func grokSnapshotAfterBillingProbe(account *Account, status int) *xai.QuotaSnapshot {
	now := time.Now().UTC().Format(time.RFC3339)
	if account != nil {
		observed, err := grokQuotaSnapshotFromExtra(account.Extra)
		if err == nil && observed != nil && observed.HasObservedHeaders() {
			cloned := *observed
			cloned.StatusCode = status
			cloned.LastProbeAt = now
			if strings.TrimSpace(cloned.ObservationSource) == "" {
				cloned.ObservationSource = "chat_response_headers"
			}
			return &cloned
		}
	}
	return &xai.QuotaSnapshot{
		StatusCode:        status,
		ObservationSource: "billing_api",
		LastProbeAt:       now,
		UpdatedAt:         now,
	}
}

func (s *GrokQuotaService) ResetQuota(ctx context.Context, accountID int64) (*GrokQuotaResetResult, error) {
	if _, err := s.loadGrokOAuthAccount(ctx, accountID); err != nil {
		return nil, err
	}
	return nil, infraerrors.New(
		http.StatusNotImplemented,
		"GROK_QUOTA_RESET_UNSUPPORTED",
		"xAI does not expose a Grok subscription quota reset endpoint for OAuth accounts",
	)
}

func (s *GrokQuotaService) prepareProbe(ctx context.Context, accountID int64) (*Account, string, string, error) {
	if s == nil || s.tokenProvider == nil || s.httpUpstream == nil {
		return nil, "", "", infraerrors.New(
			http.StatusInternalServerError,
			"GROK_QUOTA_NOT_CONFIGURED",
			"grok quota service is not configured",
		)
	}
	account, err := s.loadGrokOAuthAccount(ctx, accountID)
	if err != nil {
		return nil, "", "", err
	}
	token, err := s.tokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, "", "", infraerrors.Newf(
			http.StatusBadGateway,
			"GROK_QUOTA_TOKEN_UNAVAILABLE",
			"failed to acquire access token: %v",
			err,
		)
	}
	if strings.TrimSpace(token) == "" {
		return nil, "", "", infraerrors.New(
			http.StatusBadGateway,
			"GROK_QUOTA_TOKEN_UNAVAILABLE",
			"access token is empty",
		)
	}
	return account, token, s.resolveProxyURL(ctx, account), nil
}

func (s *GrokQuotaService) resolveProxyURL(ctx context.Context, account *Account) string {
	if account == nil || account.ProxyID == nil {
		return ""
	}
	if account.Proxy != nil {
		return account.Proxy.URL()
	}
	if s != nil && s.proxyRepo != nil {
		if proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID); err == nil && proxy != nil {
			return proxy.URL()
		}
	}
	return ""
}

func (s *GrokQuotaService) loadGrokOAuthAccount(ctx context.Context, accountID int64) (*Account, error) {
	if s == nil || s.accountRepo == nil {
		return nil, infraerrors.New(
			http.StatusInternalServerError,
			"GROK_QUOTA_NOT_CONFIGURED",
			"grok quota service is not configured",
		)
	}
	account, err := s.accountRepo.GetByID(ctx, accountID)
	if err != nil {
		return nil, infraerrors.Newf(http.StatusNotFound, "GROK_QUOTA_ACCOUNT_NOT_FOUND", "account not found: %v", err)
	}
	if account == nil {
		return nil, infraerrors.New(http.StatusNotFound, "GROK_QUOTA_ACCOUNT_NOT_FOUND", "account not found")
	}
	if account.Platform != PlatformGrok {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_INVALID_PLATFORM", "account is not a Grok account")
	}
	if account.Type != AccountTypeOAuth {
		return nil, infraerrors.New(http.StatusBadRequest, "GROK_QUOTA_INVALID_TYPE", "account is not an OAuth account")
	}
	return account, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
