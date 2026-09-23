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
	models := []struct {
		name                              string
		input, output, cacheWrite, cached float64
	}{
		{name: "gpt-6-sol", input: 4, output: 20, cacheWrite: 5, cached: 0.4},
		{name: "gpt-6-luna", input: 0.2, output: 1.2, cacheWrite: 0.25, cached: 0.02},
	}
	tokens := UsageTokens{
		InputTokens: 1000, OutputTokens: 2000, CacheCreationTokens: 3000, CacheReadTokens: 4000,
	}
	for _, source := range sources {
		t.Run(source.name, func(t *testing.T) {
			svc := NewBillingService(&config.Config{}, source.pricing)
			for _, model := range models {
				for _, name := range []string{model.name, "openai/" + model.name, model.name + "-preview"} {
					t.Run(name, func(t *testing.T) {
						cost, err := svc.CalculateCost(name, tokens, 1)
						require.NoError(t, err)
						is := assert.New(t)
						is.InDelta(model.input/1000, cost.InputCost, 1e-12)
						is.InDelta(model.output*2/1000, cost.OutputCost, 1e-12)
						is.InDelta(model.cacheWrite*3/1000, cost.CacheCreationCost, 1e-12)
						is.InDelta(model.cached*4/1000, cost.CacheReadCost, 1e-12)
						expected := (model.input + model.output*2 + model.cacheWrite*3 + model.cached*4) / 1000
						is.InDelta(expected, cost.TotalCost, 1e-12)
					})
				}
			}
		})
	}
}
