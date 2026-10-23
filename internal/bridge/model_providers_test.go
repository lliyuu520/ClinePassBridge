package bridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProviderDiscoveryIgnoresConfiguredPins(t *testing.T) {
	for _, policy := range []string{ProviderPolicyConfig, ProviderPolicyClient} {
		t.Run(policy, func(t *testing.T) {
			s := probeService(t)
			t.Cleanup(func() { observabilityShutdown(t, s) })
			s.cfg.ProviderPolicy = policy
			s.cfg.Models[0].Providers = []string{"deepseek", "wafer"}
			routing := sseFrame(map[string]any{"routing": map[string]any{
				"fallbacksAvailable": []string{"alibaba", "wafer"}, "resolvedProvider": "deepseek",
			}})
			host := &capturingHost{plan: ssePlan(routing, simpleSSE())}
			s.SetHost(host.call)

			result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
				Method: "POST", Path: apiBase + "/models/providers", HostCallbackID: "probe-callback",
				Body: jsonBytes(map[string]any{"model": "deepseek-flash", "credential_id": "probe-account"}),
			}))
			if err != nil {
				t.Fatal(err)
			}
			response := result.(ManagementResponse)
			var body struct {
				OK        bool            `json:"ok"`
				Status    int             `json:"status"`
				Providers []string        `json:"providers"`
				ServedBy  string          `json:"served_by"`
				Has       json.RawMessage `json:"has"`
			}
			if err := json.Unmarshal(response.Body, &body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || !body.OK || body.Status != 200 || body.ServedBy != "deepseek" {
				t.Fatalf("provider discovery failed: %s", response.Body)
			}
			if got := strings.Join(body.Providers, ","); got != "alibaba,wafer,deepseek" {
				t.Errorf("providers = %q, want candidates beyond the configured pins", got)
			}
			if body.Has != nil {
				t.Errorf("probe without a capability filter reported has = %s", body.Has)
			}

			host.mu.Lock()
			defer host.mu.Unlock()
			if len(host.bodies) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(host.bodies))
			}
			probe := host.bodies[0]
			for _, key := range []string{"provider", "providerOptions"} {
				if _, present := probe[key]; present {
					t.Errorf("provider discovery retained %s: %s", key, jsonBytes(probe))
				}
			}
			if probe["model"] != "cline-pass/deepseek-v4.1-flash" || probe["stream"] != true || probe["max_tokens"] != float64(64) {
				t.Errorf("unexpected probe settings: %s", jsonBytes(probe))
			}
			messages := list(probe["messages"])
			if len(messages) != 1 || object(messages[0])["role"] != "user" || object(messages[0])["content"] != "ok" {
				t.Errorf("unexpected probe messages: %s", jsonBytes(probe))
			}
		})
	}
}

func TestProviderDiscoveryPreservesConfiguredCapabilities(t *testing.T) {
	for _, policy := range []string{ProviderPolicyConfig, ProviderPolicyClient} {
		t.Run(policy, func(t *testing.T) {
			s := probeService(t)
			t.Cleanup(func() { observabilityShutdown(t, s) })
			s.cfg.ProviderPolicy = policy
			s.cfg.Models[0].Providers = []string{"deepseek", "wafer"}
			s.cfg.Models[0].Has = []string{"vision", "implicit-caching"}
			routing := sseFrame(map[string]any{"routing": map[string]any{
				"fallbacksAvailable": []string{"wafer"}, "resolvedProvider": "deepseek",
			}})
			host := &capturingHost{plan: ssePlan(routing, simpleSSE())}
			s.SetHost(host.call)
			result, err := s.Handle("management.handle", jsonBytes(ManagementRequest{
				Method: "POST", Path: apiBase + "/models/providers", HostCallbackID: "probe-callback",
				Body: jsonBytes(map[string]any{"model": "deepseek-flash", "credential_id": "probe-account"}),
			}))
			if err != nil {
				t.Fatal(err)
			}
			response := result.(ManagementResponse)
			var body map[string]any
			if err := json.Unmarshal(response.Body, &body); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 200 || body["ok"] != true || number(body["status"]) != 200 {
				t.Fatalf("provider discovery failed: %s", response.Body)
			}
			const wantHas = `["vision","implicit-caching"]`
			if got := string(jsonBytes(body["has"])); got != wantHas {
				t.Errorf("response has = %s, want %s", got, wantHas)
			}
			host.mu.Lock()
			defer host.mu.Unlock()
			if len(host.bodies) != 1 {
				t.Fatalf("upstream requests = %d, want 1", len(host.bodies))
			}
			probe := host.bodies[0]
			if _, present := probe["provider"]; present {
				t.Errorf("provider discovery retained legacy routing: %s", jsonBytes(probe))
			}
			if got := string(jsonBytes(probe["providerOptions"])); got != `{"gateway":{"has":`+wantHas+`}}` {
				t.Errorf("provider probe lost its filter or retained routing hints: %s", got)
			}
		})
	}
}

func TestProviderProbePreservesEffectiveHasPresence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		raw     string
		present bool
	}{
		{name: "absent"},
		{name: "null", raw: "null", present: true},
		{name: "empty", raw: "[]", present: true},
		{name: "capabilities", raw: `["vision","implicit-caching"]`, present: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := map[string]any{"only": []string{"deepseek"}, "order": []string{"wafer"}, "sort": "latency", "allowFallbacks": false}
			if tc.present {
				var value any
				if err := json.Unmarshal([]byte(tc.raw), &value); err != nil {
					t.Fatal(err)
				}
				gateway["has"] = value
			}
			probe := map[string]any{
				"provider":        map[string]any{"only": []string{"deepseek"}},
				"providerOptions": map[string]any{"gateway": gateway, "custom": map[string]any{"hint": true}},
				"messages":        []any{map[string]any{"role": "user", "content": "ok"}},
			}
			has, present := unpinProviderProbe(probe)
			if present != tc.present {
				t.Fatalf("has presence = %v, want %v", present, tc.present)
			}
			if _, exists := probe["provider"]; exists {
				t.Error("probe retained legacy routing")
			}
			if !tc.present {
				if _, exists := probe["providerOptions"]; exists {
					t.Errorf("probe without capabilities retained providerOptions: %s", jsonBytes(probe))
				}
			} else {
				if got := string(jsonBytes(has)); got != tc.raw {
					t.Errorf("reported has = %s, want %s", got, tc.raw)
				}
				if got := string(jsonBytes(probe["providerOptions"])); got != `{"gateway":{"has":`+tc.raw+`}}` {
					t.Errorf("probe options = %s, want only the effective has filter", got)
				}
			}
			if len(list(probe["messages"])) != 1 {
				t.Error("probe lost its messages")
			}
		})
	}
}

func TestRoutingCandidatesFinalProviderTakesPrecedence(t *testing.T) {
	for _, raw := range []string{
		`{"resolvedProvider":"wafer","finalProvider":"deepseek","fallbacksAvailable":["baseten"]}`,
		`{"finalProvider":"deepseek","resolvedProvider":"wafer","fallbacksAvailable":["baseten"]}`,
		"data: {\"resolvedProvider\":\"wafer\",\"fallbacksAvailable\":[\"baseten\"]}\n\ndata: {\"finalProvider\":\"deepseek\"}\n\n",
		`{"error":{"message":"failed: {\"resolvedProvider\":\"wafer\",\"finalProvider\":\"deepseek\",\"fallbacksAvailable\":[\"baseten\"]}"}}`,
	} {
		candidates, served := routingCandidates(raw)
		if served != "deepseek" || strings.Join(candidates, ",") != "baseten,deepseek" {
			t.Errorf("routing = %v / %q, want baseten,deepseek / deepseek; raw=%s", candidates, served, raw)
		}
	}
}

func TestRoutingCandidatesDoesNotInventAlternatives(t *testing.T) {
	for _, tc := range []struct {
		name       string
		raw        string
		candidates string
		served     string
	}{
		{name: "private route", raw: `{"resolvedProvider":"openai-compatible-private"}`, served: "openai-compatible-private"},
		{name: "empty alternatives", raw: `{"finalProvider":"deepseek","fallbacksAvailable":[]}`, served: "deepseek"},
		{name: "explicit single candidate", raw: `{"finalProvider":"deepseek","fallbacksAvailable":["deepseek"]}`, candidates: "deepseek", served: "deepseek"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidates, served := routingCandidates(tc.raw)
			if strings.Join(candidates, ",") != tc.candidates || served != tc.served {
				t.Errorf("routing = %v / %q, want %q / %q", candidates, served, tc.candidates, tc.served)
			}
		})
	}
}
