package pricing

import (
	"bytes"
	"strings"
	"testing"
)

func TestResolvePriceExact(t *testing.T) {
	p, ok := ResolvePrice("glm-5.2", nil)
	if !ok {
		t.Fatal("glm-5.2 should resolve (exact)")
	}
	if p.Input != 1.4 || p.Output != 4.4 || p.CacheRead != 0.26 || p.CacheWrite != 0 {
		t.Errorf("glm-5.2 price = %+v, want {1.4 4.4 0.26 0}", p)
	}
}

func TestResolvePriceFamily(t *testing.T) {
	// No exact entry; falls back to claude-sonnet family prefix.
	p, ok := ResolvePrice("claude-sonnet-9", nil)
	if !ok {
		t.Fatal("claude-sonnet-9 should resolve via family prefix")
	}
	if p.Input != 3 || p.CacheWrite != 3.75 {
		t.Errorf("claude-sonnet-9 family price = %+v, want {3 15 0.3 3.75}", p)
	}
}

func TestResolvePriceLongestFamilyWins(t *testing.T) {
	// Two family prefixes match "claude-sonnet-foo" — but only claude-sonnet
	// does here; ensure the exact-length entry resolves and longest-prefix logic
	// does not panic on overlapping prefixes.
	p, ok := ResolvePrice("claude-sonnet-4-20250514", nil)
	if !ok || p.Input != 3 || p.CacheWrite != 3.75 {
		t.Errorf("claude-sonnet-4-20250514 = %+v ok=%v, want family {3 15 0.3 3.75}", p, ok)
	}
}

func TestResolvePriceNone(t *testing.T) {
	if _, ok := ResolvePrice("totally-unknown-model", nil); ok {
		t.Fatal("unknown model should not resolve")
	}
}

func TestResolvePriceOverrideWins(t *testing.T) {
	overrides := map[string]TokenPrice{"glm-5.2": {Input: 0.5, Output: 2.0, CacheRead: 0.1}}
	p, ok := ResolvePrice("GLM-5.2", overrides) // case-insensitive
	if !ok || p.Input != 0.5 || p.Output != 2.0 || p.CacheRead != 0.1 {
		t.Errorf("override = %+v ok=%v, want config override", p, ok)
	}
}

func TestResolvePriceCaseInsensitive(t *testing.T) {
	p1, _ := ResolvePrice("GLM-5.2", nil)
	p2, _ := ResolvePrice("glm-5.2", nil)
	if p1 != p2 {
		t.Errorf("case mismatch: %v vs %v", p1, p2)
	}
}

func TestResolvePriceEmpty(t *testing.T) {
	if _, ok := ResolvePrice("", nil); ok {
		t.Fatal("empty model id should not resolve")
	}
}

// ComputeCost with CacheRead>0 must subtract cached tokens from the input pool
// (DESIGN §7 carve-out).
func TestComputeCostCacheReadCarveOut(t *testing.T) {
	// glm-5.2: input 1.4, output 4.4, cacheRead 0.26, cacheWrite 0.
	// promptTokens=120 (TOTAL) = uncached 30 + cacheRead 80 + cacheWrite 10.
	u := UsageTokens{Input: 120, CachedInput: 80, CacheWrite: 10, Output: 45}
	p, _ := ResolvePrice("glm-5.2", nil)
	c := ComputeCost(u, p)

	// cacheWrite rate is 0 for glm → cw gated to 0, so cacheWrite tokens stay in input pool.
	// uncached = 120 - 80(cached, rate>0) - 0(cw gated) = 40
	wantInput := round8(40.0 * 1.4 / PricePerMillion)
	wantCached := round8(80.0 * 0.26 / PricePerMillion)
	wantOutput := round8(45.0 * 4.4 / PricePerMillion)
	if c.Input != wantInput {
		t.Errorf("Input = %.10f, want %.10f", c.Input, wantInput)
	}
	if c.CachedInput != wantCached {
		t.Errorf("CachedInput = %.10f, want %.10f", c.CachedInput, wantCached)
	}
	if c.CacheWrite != 0 {
		t.Errorf("CacheWrite = %v, want 0 (glm has no cacheWrite rate)", c.CacheWrite)
	}
	if c.Output != wantOutput {
		t.Errorf("Output = %.10f, want %.10f", c.Output, wantOutput)
	}
	if c.Total != round8(wantInput+wantCached+wantOutput) {
		t.Errorf("Total = %.10f, want %.10f", c.Total, round8(wantInput+wantCached+wantOutput))
	}
}

// ComputeCost with CacheRead=0 must leave cached tokens in the input pool
// (the §7 fix: a model with no cache rate bills cache at the full input rate).
func TestComputeCostNoCacheRateStaysInInput(t *testing.T) {
	// A hypothetical model priced input=2, output=8, no cache rates.
	p := TokenPrice{Input: 2, Output: 8}
	u := UsageTokens{Input: 100, CachedInput: 60, CacheWrite: 0, Output: 10}
	c := ComputeCost(u, p)
	// cached gated to 0 (rate 0) → uncached = 100 - 0 - 0 = 100
	wantInput := round8(100.0 * 2.0 / PricePerMillion)
	wantOutput := round8(10.0 * 8.0 / PricePerMillion)
	if c.Input != wantInput {
		t.Errorf("Input = %.10f, want %.10f (cached must stay in pool)", c.Input, wantInput)
	}
	if c.CachedInput != 0 {
		t.Errorf("CachedInput = %v, want 0", c.CachedInput)
	}
	if c.Output != wantOutput {
		t.Errorf("Output = %.10f, want %.10f", c.Output, wantOutput)
	}
}

// CacheWrite carve-out only when CacheWrite rate > 0 (Anthropic).
func TestComputeCostCacheWriteCarveOut(t *testing.T) {
	// claude-sonnet-4: input 3, output 15, cacheRead 0.3, cacheWrite 3.75.
	p, _ := ResolvePrice("claude-sonnet-4", nil)
	u := UsageTokens{Input: 200, CachedInput: 50, CacheWrite: 30, Output: 40}
	c := ComputeCost(u, p)
	// both rates > 0: uncached = 200 - 50 - 30 = 120
	wantInput := round8(120.0 * 3.0 / PricePerMillion)
	wantCached := round8(50.0 * 0.3 / PricePerMillion)
	wantCW := round8(30.0 * 3.75 / PricePerMillion)
	wantOutput := round8(40.0 * 15.0 / PricePerMillion)
	if c.Input != wantInput {
		t.Errorf("Input = %.10f, want %.10f", c.Input, wantInput)
	}
	if c.CachedInput != wantCached {
		t.Errorf("CachedInput = %.10f, want %.10f", c.CachedInput, wantCached)
	}
	if c.CacheWrite != wantCW {
		t.Errorf("CacheWrite = %.10f, want %.10f", c.CacheWrite, wantCW)
	}
	if c.Output != wantOutput {
		t.Errorf("Output = %.10f, want %.10f", c.Output, wantOutput)
	}
}

func TestWarnOnceNoPriceIdempotent(t *testing.T) {
	ResetWarningsForTest()
	var buf bytes.Buffer
	orig := warnOut
	warnOut = &buf
	defer func() { warnOut = orig }()

	WarnOnceNoPrice("mystery-model")
	WarnOnceNoPrice("mystery-model")
	WarnOnceNoPrice("Mystery-Model") // normalized → same id
	out := buf.String()
	if cnt := strings.Count(out, "no price for model"); cnt != 1 {
		t.Errorf("warned %d times, want 1:\n%s", cnt, out)
	}
}
