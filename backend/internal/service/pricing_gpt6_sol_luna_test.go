package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGPT6SolLunaBilling(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("..", "..", "resources", "model-pricing", "model_prices_and_context_window.json"))
	require.NoError(t, err)
	catalog := &PricingService{}
	catalog.pricingData, err = catalog.parsePricingData(data)
	require.NoError(t, err)

	// A stale catalog must not price Sol/Luna using GPT-6 Astra or the default model.
	stale := &PricingService{pricingData: map[string]*LiteLLMModelPricing{
		"gpt-6":         openAIGPT6AstraFallbackPricing,
		"gpt-6-astra":   openAIGPT6AstraFallbackPricing,
		"gpt-5.1-codex": {InputCostPerToken: 1.25e-6, OutputCostPerToken: 10e-6},
	}}
	sources := []struct {
		name    string
		pricing *PricingService
	}{
		{name: "catalog", pricing: catalog},
		{name: "stale_catalog", pricing: stale},
		{name: "billing_fallback"},
	}
	type rates struct {
		input, output, cacheWrite, cached float64
	}
	models := []struct {
		name        string
		short, long rates
	}{
		{
			name:  "gpt-6-sol",
			short: rates{input: 2, output: 10, cacheWrite: 2.5, cached: 0.2},
			long:  rates{input: 4, output: 15, cacheWrite: 5, cached: 0.4},
		},
		{
			name:  "gpt-6-luna",
			short: rates{input: 0.1, output: 0.5, cacheWrite: 0.125, cached: 0.01},
			long:  rates{input: 0.2, output: 0.75, cacheWrite: 0.25, cached: 0.02},
		},
	}
	contexts := []struct {
		name   string
		tokens UsageTokens
		long   bool
	}{
		{
			name: "short_context",
			tokens: UsageTokens{
				InputTokens: 1000, OutputTokens: 2000, CacheCreationTokens: 3000, CacheReadTokens: 4000,
			},
		},
		{
			name: "at_threshold",
			tokens: UsageTokens{
				InputTokens: 100_000, OutputTokens: 2000, CacheCreationTokens: 100_000, CacheReadTokens: 72_000,
			},
		},
		{
			name: "above_threshold",
			tokens: UsageTokens{
				InputTokens: 100_000, OutputTokens: 2000, CacheCreationTokens: 100_000, CacheReadTokens: 72_001,
			},
			long: true,
		},
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			svc := NewBillingService(&config.Config{}, source.pricing)
			for _, model := range models {
				for _, name := range []string{model.name, "openai/" + model.name, model.name + "-preview"} {
					for _, context := range contexts {
						t.Run(name+"/"+context.name, func(t *testing.T) {
							prices := model.short
							if context.long {
								prices = model.long
							}
							tokens := context.tokens
							cost, err := svc.CalculateCost(name, tokens, 1)
							require.NoError(t, err)
							is := assert.New(t)
							inputCost := float64(tokens.InputTokens) * prices.input / 1e6
							outputCost := float64(tokens.OutputTokens) * prices.output / 1e6
							cacheWriteCost := float64(tokens.CacheCreationTokens) * prices.cacheWrite / 1e6
							cacheReadCost := float64(tokens.CacheReadTokens) * prices.cached / 1e6
							is.Equal(context.long, cost.LongContextBillingApplied)
							is.InDelta(inputCost, cost.InputCost, 1e-12)
							is.InDelta(outputCost, cost.OutputCost, 1e-12)
							is.InDelta(cacheWriteCost, cost.CacheCreationCost, 1e-12)
							is.InDelta(cacheReadCost, cost.CacheReadCost, 1e-12)
							is.InDelta(inputCost+outputCost+cacheWriteCost+cacheReadCost, cost.TotalCost, 1e-12)
						})
					}
				}
			}
		})
	}
}
