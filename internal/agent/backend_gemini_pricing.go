package agent

import "time"

// Gemini API pricing (Paid tier, "Standard" tab), USD per 1M tokens.
//
// Source: https://ai.google.dev/gemini-api/docs/pricing (last updated
// 2026-10-14), plus the model pages
// https://ai.google.dev/gemini-api/docs/generate-content/latest-model
// (Gemini 4 Argon),
// https://aistudio.google.com/docs/whats-new-gemini-4-carbon (Gemini 4
// Carbon, 2026-11-10: same rates and step-up date as Argon; paid tier
// only, implicit caching only) and
// https://ai.google.dev/gemini-api/docs/generate-content/whats-new-gemini-3.8
// (gemini-3.8-flash). Output rates include thinking tokens. None of the
// models has a long-context tier on the Standard tab, so one rate applies
// to the whole request.
//
//	gemini-4-carbon,
//	gemini-4-argon   until 2026-11-30: in $2.00 / out $10.00 / cached $0.10
//	                 from  2026-12-01: in $4.00 / out $20.00 / cached $0.20
//	gemini-3.8-flash until 2026-12-31: in $0.75 / out $3.75  / cached $0.075
//	                 from  2027-01-01: in $1.50 / out $7.50  / cached $0.15
//
// The step-up dates are the introductory-price end dates on the pricing
// page; kojo switches on the turn's completion time (UTC). Batch / Flex /
// Priority tiers and cache storage are not used by kojo and are not
// modelled.
type geminiPriceStep struct {
	from                     time.Time // inclusive; zero = since forever
	input, output, cacheRead float64   // USD per 1M tokens
}

var geminiPriceTable = map[string][]geminiPriceStep{
	"gemini-4-carbon": {
		{input: 2.00, output: 10.00, cacheRead: 0.10},
		{from: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), input: 4.00, output: 20.00, cacheRead: 0.20},
	},
	"gemini-4-argon": {
		{input: 2.00, output: 10.00, cacheRead: 0.10},
		{from: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC), input: 4.00, output: 20.00, cacheRead: 0.20},
	},
	"gemini-3.8-flash": {
		{input: 0.75, output: 3.75, cacheRead: 0.075},
		{from: time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), input: 1.50, output: 7.50, cacheRead: 0.15},
	},
}

// geminiPriceAt returns the price step for model at time at.
func geminiPriceAt(model string, at time.Time) (geminiPriceStep, bool) {
	steps, ok := geminiPriceTable[model]
	if !ok || len(steps) == 0 {
		return geminiPriceStep{}, false
	}
	cur := steps[0]
	for _, s := range steps[1:] {
		if !at.Before(s.from) {
			cur = s
		}
	}
	return cur, true
}

// geminiUsageCostUSD prices one turn's summed usage for model (the
// concrete id kojo sent). OpenCode's step_finish tokens split the prompt
// into uncached input, cache reads and cache writes; parseGeminiStream
// folds cache writes into InputTokens and reasoning into OutputTokens.
// Returns 0 (cost unset: the UI shows none) for a model without a known
// price rather than a guess. OpenCode's own cost field is not used: it
// comes from its bundled model catalogue, which does not track the
// step-up dates in geminiPriceTable.
func geminiUsageCostUSD(model string, u *Usage, at time.Time) float64 {
	if u == nil {
		return 0
	}
	p, ok := geminiPriceAt(resolveGeminiModel(model), at)
	if !ok {
		return 0
	}
	return float64(u.InputTokens)*p.input/1e6 +
		float64(u.CacheReadInputTokens)*p.cacheRead/1e6 +
		float64(u.OutputTokens)*p.output/1e6
}
