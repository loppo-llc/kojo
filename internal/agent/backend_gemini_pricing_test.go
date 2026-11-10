package agent

import (
	"math"
	"testing"
	"time"
)

func TestGeminiPriceAt_DateSwitch(t *testing.T) {
	cases := []struct {
		model            string
		at               time.Time
		in, out, cacheRd float64
	}{
		{"gemini-4-carbon", time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), 2, 10, 0.10},
		{"gemini-4-carbon", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 4, 20, 0.20},
		{"gemini-4-argon", time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC), 2, 10, 0.10},
		{"gemini-4-argon", time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), 4, 20, 0.20},
		{"gemini-3.8-flash", time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC), 0.75, 3.75, 0.075},
		{"gemini-3.8-flash", time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), 1.50, 7.50, 0.15},
	}
	for _, c := range cases {
		p, ok := geminiPriceAt(c.model, c.at)
		if !ok || p.input != c.in || p.output != c.out || p.cacheRead != c.cacheRd {
			t.Errorf("%s @ %s = %+v ok=%v", c.model, c.at, p, ok)
		}
	}
	for _, m := range []string{"flash", "pro", "auto", "gemini-3-flash-base", ""} {
		if _, ok := geminiPriceAt(m, time.Now()); ok {
			t.Errorf("geminiPriceAt(%q) should be unknown", m)
		}
	}
}

func TestGeminiUsageCostUSD(t *testing.T) {
	intro := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	later := time.Date(2026, 12, 2, 0, 0, 0, 0, time.UTC)
	u := &Usage{InputTokens: 8103, OutputTokens: 109, CacheReadInputTokens: 1000}
	want := (8103*2.0 + 1000*0.10 + 109*10.0) / 1e6
	if got := geminiUsageCostUSD("gemini-4-argon", u, intro); math.Abs(got-want) > 1e-12 {
		t.Fatalf("argon intro = %v, want %v", got, want)
	}
	// Argon's price doubles on 12/1.
	if got := geminiUsageCostUSD("gemini-4-argon", u, later); math.Abs(got-2*want) > 1e-12 {
		t.Fatalf("argon after step-up = %v, want %v", got, 2*want)
	}
	// The default model ("") and the legacy alias resolve before pricing.
	if got := geminiUsageCostUSD("", u, intro); math.Abs(got-want) > 1e-12 {
		t.Fatalf("default model = %v", got)
	}
	flash := (8103*0.75 + 1000*0.075 + 109*3.75) / 1e6
	if got := geminiUsageCostUSD("flash", u, intro); math.Abs(got-flash) > 1e-12 {
		t.Fatalf("flash alias = %v, want %v", got, flash)
	}
	if got := geminiUsageCostUSD("gemini-3-flash-base", u, intro); got != 0 {
		t.Fatalf("unknown model priced: %v", got)
	}
	if got := geminiUsageCostUSD("gemini-4-argon", nil, intro); got != 0 {
		t.Fatalf("nil usage = %v", got)
	}
}
