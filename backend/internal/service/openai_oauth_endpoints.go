package service

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/util/urlvalidator"
)

func (a *Account) OpenAIOAuthEndpoints() openai.OAuthEndpoints {
	if a == nil || a.Platform != PlatformOpenAI || !a.IsOpenAIOAuthLike() {
		return openai.OAuthEndpoints{}
	}
	e, err := openai.OAuthEndpointsFromCredentials(a.Credentials)
	if err != nil {
		// Corrupt persisted settings must fail closed instead of using official hosts.
		return openai.OAuthEndpoints{AuthBaseURL: "://invalid", ChatGPTBaseURL: "://invalid", ResponsesURL: "://invalid"}
	}
	return e
}

func (a *Account) OpenAIOAuthURL(officialURL string) string {
	return a.OpenAIOAuthEndpoints().Rewrite(officialURL)
}

func validateOpenAIOAuthEndpoints(e openai.OAuthEndpoints, cfg *config.Config) error {
	if err := e.Validate(); err != nil {
		return infraerrors.BadRequest("INVALID_OPENAI_OAUTH_ENDPOINT", err.Error())
	}
	for _, raw := range e.Values() {
		if raw == "" || raw == openai.DefaultResponsesURL || raw == openai.DefaultChatGPTBaseURL || raw == openai.DefaultAuthBaseURL || raw == openai.DefaultPlatformBaseURL {
			continue
		}
		allowHTTP := cfg == nil || cfg.Security.URLAllowlist.AllowInsecureHTTP
		var err error
		if cfg != nil && cfg.Security.URLAllowlist.Enabled {
			_, err = urlvalidator.ValidateHTTPURL(raw, allowHTTP, urlvalidator.ValidationOptions{
				AllowedHosts:     cfg.Security.URLAllowlist.UpstreamHosts,
				RequireAllowlist: true,
				AllowPrivate:     cfg.Security.URLAllowlist.AllowPrivateHosts,
			})
		} else {
			_, err = urlvalidator.ValidateURLFormat(raw, allowHTTP)
		}
		if err != nil {
			return infraerrors.BadRequest("INVALID_OPENAI_OAUTH_ENDPOINT", err.Error())
		}
	}
	return nil
}

func validateOpenAIOAuthEndpointCredentials(platform, accountType string, credentials map[string]any, cfg *config.Config) error {
	if platform != PlatformOpenAI || (accountType != AccountTypeOAuth && accountType != AccountTypeSetupToken) {
		return nil
	}
	e, err := openai.OAuthEndpointsFromCredentials(credentials)
	if err != nil {
		return infraerrors.BadRequest("INVALID_OPENAI_OAUTH_ENDPOINT", err.Error())
	}
	return validateOpenAIOAuthEndpoints(e, cfg)
}

func (s *OpenAIGatewayService) openAIOAuthTarget(ctx context.Context, account *Account, officialURL string) (string, error) {
	credentialAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
	if err != nil {
		return "", err
	}
	endpoints := credentialAccount.OpenAIOAuthEndpoints()
	if err := validateOpenAIOAuthEndpoints(endpoints, s.cfg); err != nil {
		return "", err
	}
	return endpoints.Rewrite(officialURL), nil
}
