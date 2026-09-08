package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestAccountTestServiceValidateUpstreamBaseURLAllowsConfiguredHTTPWithAllowlist(t *testing.T) {
	svc := &AccountTestService{cfg: &config.Config{
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled:           true,
			AllowInsecureHTTP: true,
			UpstreamHosts:     []string{"relay.example.com"},
		}},
	}}

	got, err := svc.validateUpstreamBaseURL("http://relay.example.com/v1")
	require.NoError(t, err)
	require.Equal(t, "http://relay.example.com/v1", got)
}

func TestAccountTestServiceValidateUpstreamBaseURLStillRejectsHTTPWhenDisabled(t *testing.T) {
	svc := &AccountTestService{cfg: &config.Config{
		Security: config.SecurityConfig{URLAllowlist: config.URLAllowlistConfig{
			Enabled:       true,
			UpstreamHosts: []string{"relay.example.com"},
		}},
	}}

	_, err := svc.validateUpstreamBaseURL("http://relay.example.com/v1")
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid url scheme: http")
}
