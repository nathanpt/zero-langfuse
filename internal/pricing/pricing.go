// Package pricing resolves per-token prices and computes cache-aware USD cost
// from token usage (DESIGN §7).
//
// It ports omp-langfuse's src/pricing.ts table, but the cost formula is Zero's
// corrected carve-out (DESIGN §7): promptTokens is the TOTAL input
// (uncached + cache-read + cache-write), so BOTH cache subsets are subtracted
// from the input pool — but only when their rate is > 0 (a model priced without
// a cache rate leaves those tokens billed at the full input rate). This matches
// Zero's own modelregistry.CalculateCost (cost.go:80-94).
//
// Price resolution precedence (first match wins), per metadata.modelId:
//  1. User config override (exact, case-insensitive),
//  2. Bundled exact match,
//  3. Bundled family prefix (longest match),
//  4. None (caller omits costDetails; WarnOnceNoPrice fires once).
//
// There is deliberately NO registry/catalog step (DESIGN §7 guardrail): Zero
// exposes no cost on any external surface, so the off-by-1e6 bug omp had cannot
// recur.
package pricing

import (
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
)

// PricePerMillion is the unit prices are quoted in (USD per 1M tokens).
const PricePerMillion = 1_000_000.0

// TokenPrice is USD per 1M tokens for each token class. Missing components
// default to 0 (a model with no cache pricing bills cache tokens at input rate,
// per the carve-out gate in ComputeCost).
type TokenPrice struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
}

// CostBreakdown is the per-class USD cost for a single generation.
type CostBreakdown struct {
	Input       float64
	CachedInput float64
	CacheWrite  float64
	Output      float64
	Total       float64
}

// UsageTokens is the token usage for one provider_usage event. Input is the
// TOTAL input (promptTokens — uncached + cache-read + cache-write, per the
// DESIGN §7 / Q2 finding).
type UsageTokens struct {
	Input       int
	CachedInput int
	CacheWrite  int
	Output      int
}

// bundledExact is the starter price table, USD per 1M tokens. Ported verbatim
// from omp-langfuse src/pricing.ts:68-100.
var bundledExact = map[string]TokenPrice{
	// DeepSeek (official API)
	"deepseek-v3":       {0.27, 1.1, 0.07, 0},
	"deepseek-v3.1":     {0.27, 1.1, 0.07, 0},
	"deepseek-chat":     {0.27, 1.1, 0.07, 0},
	"deepseek-r1":       {0.55, 2.19, 0.14, 0},
	"deepseek-reasoner": {0.55, 2.19, 0.14, 0},
	// Zhipu GLM-4.6 (sourced from Deep Infra / OpenRouter listings)
	"glm-4.6":   {0.43, 1.74, 0.08, 0},
	"glm-4-32b": {0.43, 1.74, 0.08, 0},
	// Zhipu GLM-5.x (catalog zeroes these as subscription models; API rates
	// sourced from Z.ai / bigmodel.cn listings). cacheWrite not published for GLM (0).
	"glm-5":   {1.4, 4.4, 0.26, 0},
	"glm-5.1": {1.4, 4.4, 0.26, 0},
	"glm-5.2": {1.4, 4.4, 0.26, 0},
	// Anthropic Claude
	"claude-sonnet-4":   {3, 15, 0.3, 3.75},
	"claude-3-5-sonnet": {3, 15, 0.3, 3.75},
	"claude-3-7-sonnet": {3, 15, 0.3, 3.75},
	"claude-haiku-4":    {1, 5, 0.1, 1.25},
	"claude-3-5-haiku":  {1, 5, 0.1, 1.25},
	"claude-opus-4":     {15, 75, 1.5, 18.75},
	"claude-3-opus":     {15, 75, 1.5, 18.75},
	// OpenAI
	"gpt-4.1":      {2, 8, 0.5, 0},
	"gpt-4.1-mini": {0.4, 1.6, 0.1, 0},
	"gpt-4.1-nano": {0.1, 0.4, 0.025, 0},
	"gpt-4o":       {2.5, 10, 1.25, 0},
	"gpt-4o-mini":  {0.15, 0.6, 0.075, 0},
	"o3-mini":      {1.1, 4.4, 0.55, 0},
	"o4-mini":      {1.1, 4.4, 0.55, 0},
}

// bundledFamily is checked longest-prefix-first after exact matches fail. Used
// as estimates; config overrides are the correction path. Ported from
// pricing.ts:106-113.
var bundledFamily = []struct {
	Prefix string
	Price  TokenPrice
}{
	{"glm", TokenPrice{1.4, 4.4, 0.26, 0}},
	{"deepseek", TokenPrice{0.27, 1.1, 0.07, 0}},
	{"claude-sonnet", TokenPrice{3, 15, 0.3, 3.75}},
	{"claude-haiku", TokenPrice{1, 5, 0.1, 1.25}},
	{"claude-opus", TokenPrice{15, 75, 1.5, 18.75}},
}

// ResolvePrice resolves a per-token price for the given model id. Precedence:
// (1) overrides exact (case-insensitive), (2) bundledExact exact,
// (3) longest matching bundledFamily prefix, (4) none (ok=false).
//
// There is no registry/catalog step (DESIGN §7 guardrail).
func ResolvePrice(modelID string, overrides map[string]TokenPrice) (TokenPrice, bool) {
	id := normalizeModelID(modelID)
	if id == "" {
		return TokenPrice{}, false
	}

	// 1. user config override (exact)
	if p, ok := pickOverride(overrides, id); ok {
		return p, true
	}

	// 2. bundled exact
	if p, ok := bundledExact[id]; ok {
		return p, true
	}

	// 3. bundled family prefix (longest match)
	var best *TokenPrice
	bestLen := -1
	for i := range bundledFamily {
		e := &bundledFamily[i]
		if strings.HasPrefix(id, e.Prefix) && len(e.Prefix) > bestLen {
			best = &e.Price
			bestLen = len(e.Prefix)
		}
	}
	if best != nil {
		return *best, true
	}

	return TokenPrice{}, false
}

// pickOverride returns the override for id (case-insensitive). Config keys may
// be partial; missing components default to 0.
func pickOverride(overrides map[string]TokenPrice, id string) (TokenPrice, bool) {
	if overrides == nil {
		return TokenPrice{}, false
	}
	if p, ok := overrides[id]; ok {
		return p, true
	}
	return TokenPrice{}, false
}

// ComputeCost computes USD cost from token usage and a resolved price, using
// Zero's corrected carve-out (DESIGN §7): a cache subset is only carved out of
// the input pool when its rate is > 0 (a model priced without a cache rate bills
// those tokens at the full input rate). promptTokens (UsageTokens.Input) is the
// TOTAL input, so both subsets are subtracted from the uncached pool.
func ComputeCost(u UsageTokens, p TokenPrice) CostBreakdown {
	input := imax(0, u.Input)
	output := imax(0, u.Output)

	cached := 0
	if p.CacheRead > 0 {
		cached = imax(0, u.CachedInput)
	}
	cw := 0
	if p.CacheWrite > 0 {
		cw = imax(0, u.CacheWrite)
	}
	// Both subsets are subtracted from the input pool (DESIGN §7).
	uncached := imax(0, input-cached-cw)

	inputCost := float64(uncached) * p.Input / PricePerMillion
	cachedCost := float64(cached) * p.CacheRead / PricePerMillion
	cacheWriteCost := float64(cw) * p.CacheWrite / PricePerMillion
	outputCost := float64(output) * p.Output / PricePerMillion

	return CostBreakdown{
		Input:       round8(inputCost),
		CachedInput: round8(cachedCost),
		CacheWrite:  round8(cacheWriteCost),
		Output:      round8(outputCost),
		Total:       round8(inputCost + cachedCost + cacheWriteCost + outputCost),
	}
}

func round8(v float64) float64 {
	return math.Round(v*1e8) / 1e8
}

func imax(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func normalizeModelID(id string) string {
	return strings.ToLower(strings.TrimSpace(id))
}

var (
	warnMu       sync.Mutex
	warnedModels = map[string]bool{}
	// warnOut is where no-price warnings are written (os.Stderr in prod,
	// swappable in tests).
	warnOut io.Writer = os.Stderr
)

// WarnOnceNoPrice prints a one-time stderr warning that a model has no
// resolvable price; idempotent per (normalized) model id.
func WarnOnceNoPrice(modelID string) {
	id := normalizeModelID(modelID)
	if id == "" {
		return
	}
	warnMu.Lock()
	defer warnMu.Unlock()
	if warnedModels[id] {
		return
	}
	warnedModels[id] = true
	fmt.Fprintf(warnOut, "zero-langfuse: no price for model %q; cost omitted. Add an override in config under \"pricing\".\n", modelID)
}

// ResetWarningsForTest clears the warned-models set (test helper).
func ResetWarningsForTest() {
	warnMu.Lock()
	defer warnMu.Unlock()
	warnedModels = map[string]bool{}
}
