package bridge

import (
	"strings"
	"testing"
)

// The capability filter (`has`) is what actually moves a Cline Pass request off
// the private relay: the routing allow-list (`only`) is a hint the account may
// be unable to honour. These tests pin the merge rules so a future refactor
// cannot silently drop the field.

func TestConfiguredHasIsInjected(t *testing.T) {
	s, err := registerService(t, modelConfig(t, "    has:\n      - vision\n      - implicit-caching\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := strings.Join(slugs(gateway["has"]), ","); got != "vision,implicit-caching" {
		t.Errorf("has = %q, want vision,implicit-caching", got)
	}
	if got := strings.Join(slugs(gateway["only"]), ","); got != "deepseek" {
		t.Errorf("only = %q, want the seeded fresh-install default deepseek", got)
	}
}

func TestClientHasWinsUnderClientPolicy(t *testing.T) {
	// The caller that knows what it needs keeps control; the configured filter
	// must not overwrite a caller-supplied one.
	s, err := registerService(t, modelConfig(t, "    has:\n      - vision\nprovider_policy: client\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"has":["implicit-caching"]}}}`)
	if got := strings.Join(slugs(gateway["has"]), ","); got != "implicit-caching" {
		t.Errorf("has = %q, want the client value implicit-caching", got)
	}
}

func TestClientHasIsDroppedUnderConfigPolicy(t *testing.T) {
	// Config policy owns the routing fields, so a caller hint is discarded —
	// this is exactly why a server-side filter has to be configured before
	// switching the policy, or the request falls back to the private relay.
	s, err := registerService(t, modelConfig(t, "    has:\n      - vision\nprovider_policy: config\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}],"providerOptions":{"gateway":{"has":["implicit-caching"]}}}`)
	if got := strings.Join(slugs(gateway["has"]), ","); got != "vision" {
		t.Errorf("has = %q, want the configured value vision", got)
	}
}

func TestClearedHasLeavesNoProviderOptions(t *testing.T) {
	// An explicit empty list means the operator cleared the filter; it must not
	// be re-seeded the way the provider allow-list is.
	s, err := registerService(t, modelConfig(t, "    has: []\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	body := upstreamBodyOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if gateway := object(object(body["providerOptions"])["gateway"]); gateway != nil {
		if _, ok := gateway["has"]; ok {
			t.Errorf("a cleared filter must not be re-seeded, gateway = %v", gateway)
		}
	}
}

func TestHasIsNormalisedToLowercaseSlugs(t *testing.T) {
	s, err := registerService(t, modelConfig(t, "    has:\n      -  Vision\n      - \" implicit-caching \"\n"))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	gateway := gatewayOf(t, s, `{"messages":[{"role":"user","content":"hello"}]}`)
	if got := strings.Join(slugs(gateway["has"]), ","); got != "vision,implicit-caching" {
		t.Errorf("has = %q, want lowercased trimmed slugs", got)
	}
}

func TestInvalidHasIsRejected(t *testing.T) {
	for name, extra := range map[string]string{
		"whitespace": "    has:\n      - \"two words\"\n",
		"empty":      "    has:\n      - \"\"\n",
		"duplicate":  "    has:\n      - vision\n      - Vision\n",
		"too many":   "    has:\n      - a\n      - b\n      - c\n      - d\n      - e\n      - f\n      - g\n      - h\n      - i\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := registerService(t, modelConfig(t, extra)); err == nil {
				t.Fatalf("expected the config to be rejected")
			}
		})
	}
}
