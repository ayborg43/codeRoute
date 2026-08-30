package provider

import (
	"encoding/json"
	"math"
	"testing"
)

func parsePricing(t *testing.T, body string) *pricing {
	t.Helper()

	var p pricing
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatalf("unmarshal %s: %v", body, err)
	}
	return &p
}

// OpenRouter publishes per-token decimal strings under prompt/completion.
func TestPricingOpenRouterShape(t *testing.T) {
	p := parsePricing(t, `{"prompt":"0.000000834","completion":"0.000002501"}`)

	in, out, ok := p.perMillion()
	if !ok {
		t.Fatal("OpenRouter pricing was not recognised")
	}
	if math.Abs(in-0.834) > 1e-9 || math.Abs(out-2.501) > 1e-9 {
		t.Errorf("got %v/%v per 1M, want 0.834/2.501", in, out)
	}
}

// xKiro publishes numbers under input/output with an explicit per-million unit.
func TestPricingPerMillionShape(t *testing.T) {
	p := parsePricing(t, `{"currency":"USD","unit":"per_1m_tokens","input":0.75,"output":1.5,"cache_read":0.15}`)

	in, out, ok := p.perMillion()
	if !ok {
		t.Fatal("per-million pricing was not recognised")
	}
	if in != 0.75 || out != 1.5 {
		t.Errorf("got %v/%v, want 0.75/1.5 — the unit says these are already per million", in, out)
	}
}

// A zero price is a real price, and must survive as free rather than unknown.
func TestPricingZeroIsFreeNotUnknown(t *testing.T) {
	for _, body := range []string{
		`{"prompt":"0","completion":"0"}`,
		`{"unit":"per_1m_tokens","input":0,"output":0}`,
	} {
		p := parsePricing(t, body)
		in, out, ok := p.perMillion()
		if !ok {
			t.Errorf("%s was treated as unpriced", body)
			continue
		}
		if in != 0 || out != 0 {
			t.Errorf("%s gave %v/%v", body, in, out)
		}
	}
}

// The distinction the whole free-only feature rests on: nothing published means
// unknown, never zero.
func TestPricingUnknownIsNotFree(t *testing.T) {
	cases := map[string]string{
		"absent block":    `{}`,
		"empty strings":   `{"prompt":"","completion":""}`,
		"nulls":           `{"prompt":null,"completion":null}`,
		"only one side":   `{"prompt":"0.000001"}`,
		"unparseable":     `{"prompt":"free","completion":"free"}`,
		"unitless in/out": `{"input":0.75,"output":1.5}`,
		"other currency":  `{"currency":"EUR","unit":"per_1m_tokens","input":0,"output":0}`,
	}
	for name, body := range cases {
		p := parsePricing(t, body)
		if _, _, ok := p.perMillion(); ok {
			t.Errorf("%s (%s) was accepted as a known price", name, body)
		}
	}

	if _, _, ok := (*pricing)(nil).perMillion(); ok {
		t.Error("a nil pricing block reported a known price")
	}
}

// An unset unit on input/output is genuinely ambiguous, and guessing either way
// would misreport cost by six orders of magnitude.
func TestPricingRefusesToGuessScale(t *testing.T) {
	p := parsePricing(t, `{"input":0.75,"output":1.5}`)
	if _, _, ok := p.perMillion(); ok {
		t.Error("unitless input/output was assigned a scale by guesswork")
	}

	// With the unit stated as per-token, the same numbers scale up.
	scaled := parsePricing(t, `{"unit":"per_token","input":0.0000008,"output":0.0000024}`)
	in, out, ok := scaled.perMillion()
	if !ok {
		t.Fatal("per-token unit was not recognised")
	}
	if math.Abs(in-0.8) > 1e-9 || math.Abs(out-2.4) > 1e-9 {
		t.Errorf("got %v/%v, want 0.8/2.4", in, out)
	}
}

func TestFlexNumberAcceptsBothForms(t *testing.T) {
	var asString, asNumber flexNumber
	if err := json.Unmarshal([]byte(`"1.25"`), &asString); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`1.25`), &asNumber); err != nil {
		t.Fatal(err)
	}
	if !asString.Set || !asNumber.Set || asString.Value != 1.25 || asNumber.Value != 1.25 {
		t.Errorf("string=%+v number=%+v", asString, asNumber)
	}

	// Junk is absent, not an error that would fail the whole model list.
	var junk flexNumber
	if err := json.Unmarshal([]byte(`"n/a"`), &junk); err != nil {
		t.Errorf("junk price returned an error: %v", err)
	}
	if junk.Set {
		t.Error("junk price was recorded as set")
	}
}

// Google's model list mixes chat models with embedding, image, audio and
// question-answering endpoints. Routing to one of those earns a 400, so the
// declared methods decide what is offered.
func TestSupportsChat(t *testing.T) {
	chat := [][]string{
		{"generateContent", "countTokens"},
		{"streamGenerateContent"},
		{"countTokens", "generateContent", "batchGenerateContent"},
		nil, // provider did not say; not grounds for exclusion
		{},
	}
	for _, methods := range chat {
		if !supportsChat(methods) {
			t.Errorf("%v was excluded but can serve chat", methods)
		}
	}

	notChat := [][]string{
		{"embedContent"},
		{"predict"},
		{"generateAnswer"},      // aqa
		{"bidiGenerateContent"}, // live audio only
		{"embedContent", "countTokens"},
	}
	for _, methods := range notChat {
		if supportsChat(methods) {
			t.Errorf("%v was offered for chat but cannot serve it", methods)
		}
	}
}
