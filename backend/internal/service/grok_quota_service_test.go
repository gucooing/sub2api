//go:build unit

package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/stretchr/testify/require"
)

type grokQuotaAccountRepo struct {
	*mockAccountRepoForPlatform
	updates               map[int64]map[string]any
	updateCalls           int
	rateLimitedCalls      int
	lastRateLimitedID     int64
	lastRateLimitResetAt  time.Time
	tempUnschedCalls      int
	lastTempUnschedID     int64
	lastTempUnschedUntil  time.Time
	lastTempUnschedReason string
}

func (r *grokQuotaAccountRepo) UpdateExtra(_ context.Context, id int64, updates map[string]any) error {
	r.updateCalls++
	if r.updates == nil {
		r.updates = make(map[int64]map[string]any)
	}
	r.updates[id] = updates
	return nil
}

func (r *grokQuotaAccountRepo) SetRateLimited(_ context.Context, id int64, resetAt time.Time) error {
	r.rateLimitedCalls++
	r.lastRateLimitedID = id
	r.lastRateLimitResetAt = resetAt
	return nil
}

func (r *grokQuotaAccountRepo) SetRateLimitedIfLater(ctx context.Context, id int64, resetAt time.Time) error {
	return r.SetRateLimited(ctx, id, resetAt)
}

func (r *grokQuotaAccountRepo) SetTempUnschedulable(_ context.Context, id int64, until time.Time, reason string) error {
	r.tempUnschedCalls++
	r.lastTempUnschedID = id
	r.lastTempUnschedUntil = until
	r.lastTempUnschedReason = reason
	return nil
}

type grokQuotaProxyRepo struct {
	proxyRepoStub
	proxies map[int64]*Proxy
	calls   int
}

func (r *grokQuotaProxyRepo) GetByID(_ context.Context, id int64) (*Proxy, error) {
	r.calls++
	return r.proxies[id], nil
}

func TestGrokQuotaServiceProbeUsageUsesReadOnlyBillingRequests(t *testing.T) {
	t.Parallel()

	account := &Account{
		ID:          42,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"user_id":      "user-42",
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{42: account},
		},
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body: io.NopCloser(strings.NewReader(
				`{"config":{"currentPeriod":{"type":"USAGE_PERIOD_TYPE_WEEKLY","end":"2033-05-18T03:33:20Z"}}}`,
			)),
		},
		{
			StatusCode: http.StatusOK,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(`{"config":{"monthlyLimit":{"val":15000},"used":{"val":387}}}`)),
		},
	}}

	result, err := NewGrokQuotaService(
		repo,
		nil,
		NewGrokTokenProvider(repo, nil),
		upstream,
	).ProbeUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, "billing_api", result.Source)
	require.Nil(t, result.Credits.CreditUsagePercent)
	require.False(t, result.HeadersObserved)
	require.Nil(t, result.Snapshot.Requests)
	require.Len(t, upstream.requests, 2)
	require.Equal(t, http.MethodGet, upstream.requests[0].Method)
	require.Equal(t, xai.DefaultCLIBaseURL+"/billing?format=credits", upstream.requests[0].URL.String())
	require.Equal(t, xai.DefaultCLIBaseURL+"/billing", upstream.requests[1].URL.String())
	require.Equal(t, "Bearer access-token", upstream.requests[0].Header.Get("Authorization"))
	require.Equal(t, grokCLITokenAuth, upstream.requests[0].Header.Get("X-XAI-Token-Auth"))
	require.Equal(t, grokCLIClientVersion, upstream.requests[0].Header.Get("x-grok-client-version"))
	require.Equal(t, "user-42", upstream.requests[0].Header.Get("x-userid"))
	require.Equal(t, 15000.0, result.Monthly.MonthlyLimit.Val)
}

func TestGrokQuotaServiceProbeUsageResolvesUserIDBeforeBilling(t *testing.T) {
	t.Parallel()

	account := &Account{
		ID:          43,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{43: account},
		},
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"userId":"resolved-user"}`))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"config":{}}`))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"config":{}}`))},
	}}

	_, err := NewGrokQuotaService(
		repo,
		nil,
		NewGrokTokenProvider(repo, nil),
		upstream,
	).ProbeUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Len(t, upstream.requests, 3)
	require.Equal(t, xai.DefaultCLIBaseURL+"/user", upstream.requests[0].URL.String())
	require.Equal(t, "resolved-user", upstream.requests[1].Header.Get("x-userid"))
	require.Equal(t, "resolved-user", account.Credentials["user_id"])
}

func TestGrokQuotaServiceProbeUsagePreservesConversationQuota(t *testing.T) {
	t.Parallel()

	requestLimit := int64(21)
	requestRemaining := int64(17)
	observedAt := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	account := &Account{
		ID:       44,
		Platform: PlatformGrok,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"user_id":      "user-44",
		},
		Extra: map[string]any{
			grokQuotaSnapshotExtraKey: &xai.QuotaSnapshot{
				Requests: &xai.QuotaWindow{
					Limit:     &requestLimit,
					Remaining: &requestRemaining,
				},
				HeadersObserved:   true,
				ObservationSource: "chat_response_headers",
				LastHeadersSeenAt: observedAt,
				UpdatedAt:         observedAt,
			},
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{44: account},
		},
	}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"config":{"creditUsagePercent":30}}`))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"config":{}}`))},
	}}

	result, err := NewGrokQuotaService(
		repo,
		nil,
		NewGrokTokenProvider(repo, nil),
		upstream,
	).ProbeUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.True(t, result.HeadersObserved)
	require.EqualValues(t, requestLimit, *result.Snapshot.Requests.Limit)
	require.EqualValues(t, requestRemaining, *result.Snapshot.Requests.Remaining)
	require.Equal(t, observedAt, result.Snapshot.LastHeadersSeenAt)
}

func TestGrokQuotaServiceProbeUsageLoadsProxy(t *testing.T) {
	t.Parallel()

	proxyID := int64(7)
	account := &Account{
		ID:          45,
		Platform:    PlatformGrok,
		Type:        AccountTypeOAuth,
		Concurrency: 1,
		ProxyID:     &proxyID,
		Credentials: map[string]any{
			"access_token": "access-token",
			"expires_at":   time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"user_id":      "user-45",
		},
	}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{45: account},
		},
	}
	proxyRepo := &grokQuotaProxyRepo{proxies: map[int64]*Proxy{
		proxyID: {
			ID:       proxyID,
			Protocol: "http",
			Host:     "proxy.test",
			Port:     3128,
		},
	}}
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"config":{}}`))},
		{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"config":{}}`))},
	}}

	_, err := NewGrokQuotaService(
		repo,
		proxyRepo,
		NewGrokTokenProvider(repo, nil),
		upstream,
	).ProbeUsage(context.Background(), account.ID)
	require.NoError(t, err)
	require.Equal(t, 1, proxyRepo.calls)
	require.Equal(t, "http://proxy.test:3128", upstream.lastProxyURL)
}

func TestGrokQuotaServiceResetQuotaUnsupported(t *testing.T) {
	t.Parallel()

	account := &Account{ID: 46, Platform: PlatformGrok, Type: AccountTypeOAuth}
	repo := &grokQuotaAccountRepo{
		mockAccountRepoForPlatform: &mockAccountRepoForPlatform{
			accountsByID: map[int64]*Account{46: account},
		},
	}
	_, err := NewGrokQuotaService(repo, nil, nil, nil).ResetQuota(context.Background(), account.ID)
	require.Error(t, err)
	require.Equal(t, http.StatusNotImplemented, infraerrors.Code(err))
	require.Equal(t, "GROK_QUOTA_RESET_UNSUPPORTED", infraerrors.Reason(err))
}

func TestShouldAutoPauseGrokAccountByQuota(t *testing.T) {
	t.Parallel()

	zero := int64(0)
	limit := int64(10)
	resetFuture := time.Now().Add(time.Minute).Unix()
	retryAfter := 30
	tests := []struct {
		name     string
		snapshot xai.QuotaSnapshot
		want     bool
	}{
		{
			name: "remaining requests exhausted",
			snapshot: xai.QuotaSnapshot{
				Requests:  &xai.QuotaWindow{Limit: &limit, Remaining: &zero, ResetUnix: &resetFuture},
				UpdatedAt: time.Now().UTC().Format(time.RFC3339),
			},
			want: true,
		},
		{
			name: "retry after active",
			snapshot: xai.QuotaSnapshot{
				RetryAfterSeconds: &retryAfter,
				UpdatedAt:         time.Now().UTC().Format(time.RFC3339),
			},
			want: true,
		},
		{
			name: "stale snapshot ignored",
			snapshot: xai.QuotaSnapshot{
				Requests:  &xai.QuotaWindow{Limit: &limit, Remaining: &zero, ResetUnix: &resetFuture},
				UpdatedAt: time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			account := &Account{
				Platform: PlatformGrok,
				Type:     AccountTypeOAuth,
				Extra: map[string]any{
					grokQuotaSnapshotExtraKey: tt.snapshot,
				},
			}
			got, _ := shouldAutoPauseGrokAccountByQuota(account)
			require.Equal(t, tt.want, got)
		})
	}
}
