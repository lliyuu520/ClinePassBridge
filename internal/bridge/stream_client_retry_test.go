package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestStreamServiceErrorRetryCompatibilityBoundaries(t *testing.T) {
	for _, fault := range []struct {
		name string
		err  error
	}{
		{"service", classifyUpstreamError(200, nil, map[string]any{"error": "Stream error occurred"}, time.Now())},
		{"eof", typedTransportFault(nil, io.ErrUnexpectedEOF, "egress upstream response interrupted")},
	} {
		t.Run(fault.name, func(t *testing.T) { checkStreamRetryCompatibilityBoundaries(t, fault.err) })
	}
}

func checkStreamRetryCompatibilityBoundaries(t *testing.T, retryableErr error) {
	t.Helper()
	const original = "Stream error occurred"
	for _, tc := range []struct {
		name, path, phase, action string
		disabled, compatible      bool
		err                       error
	}{
		{name: "chat retryable error", path: "/v1/chat/completions", phase: "after_output", compatible: true, err: retryableErr},
		{name: "wrapped retryable error", path: "/v1/chat/completions", phase: "after_output", compatible: true, err: fmt.Errorf("wrapped: %w", retryableErr)},
		{name: "compatibility disabled", path: "/v1/chat/completions", phase: "after_output", disabled: true, err: retryableErr},
		{name: "missing request path", phase: "after_output", err: retryableErr},
		{name: "Claude protocol", path: "/v1/messages", phase: "after_output", err: retryableErr},
		{name: "Responses protocol", path: "/v1/responses", phase: "after_output", err: retryableErr},
		{name: "before output", path: "/v1/chat/completions", phase: "before_output", err: retryableErr},
		{name: "protocol failure", path: "/v1/chat/completions", phase: "after_output", err: upstreamProtocolFailure(original)},
		{name: "unclassified local failure", path: "/v1/chat/completions", phase: "after_output", err: fail(502, original)},
		{name: "non-service status", path: "/v1/chat/completions", phase: "after_output", err: requestFault(418, upstreamServiceKind, original)},
		{name: "authentication", path: "/v1/chat/completions", phase: "after_output", err: classifyUpstreamError(401, nil, map[string]any{"error": original}, time.Now())},
		{name: "account quota", path: "/v1/chat/completions", phase: "after_output", err: classifyUpstreamError(502, nil, map[string]any{"error": "reached your weekly Clinepass limit"}, time.Now())},
		{name: "model failure", path: "/v1/chat/completions", phase: "after_output", err: classifyUpstreamError(502, nil, map[string]any{"error": map[string]any{"code": "model_not_found", "message": original}}, time.Now())},
		{name: "account scoped service error", path: "/v1/chat/completions", phase: "after_output", err: &upstreamError{APIError: &APIError{Status: 502, Kind: upstreamServiceKind, Message: original}, Scope: "account"}},
		{name: "account scoped EOF", path: "/v1/chat/completions", phase: "after_output", err: &upstreamError{APIError: &APIError{Status: 502, Kind: upstreamNetworkKind, Message: original}, Scope: "account", TransportKind: "eof"}},
		{name: "explicit stop", path: "/v1/chat/completions", phase: "after_output", action: "stop", err: retryableErr},
		{name: "explicit continue", path: "/v1/chat/completions", phase: "after_output", action: "continue", err: retryableErr},
		{name: "explicit cooldown", path: "/v1/chat/completions", phase: "after_output", action: "continue-and-cooldown", err: retryableErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewService()
			s.cfg.ClientRetryCompatibility = !tc.disabled
			s.errorRules = nil
			if tc.action != "" {
				s.errorRules = []RequestErrorRule{{Status: 502, Match: []string{retryableErr.Error()}, Action: tc.action}}
			}
			h := newFakeHost()
			closes := 0
			var details streamErrorDetails
			s.SetHost(func(method string, payload, out any) error {
				if method != "host.stream.close" {
					t.Fatalf("terminal error unexpectedly used callback %s", method)
				}
				closes++
				details = payload.(map[string]any)["error_details"].(streamErrorDetails)
				return h.call(method, payload, out)
			})
			legacyMessage, _ := s.hostErrorDetails(tc.err, true)
			rpcMessage, rpcStatus := s.HostErrorDetails(tc.err)
			entry := LogEntry{ErrorPhase: tc.phase}
			s.closeOutputStream("fixture", tc.path, &entry, tc.err)
			if closes != 1 || details.HTTPStatus != statusOf(tc.err) || details.Phase != tc.phase {
				t.Fatalf("stream close changed diagnostic protocol: closes=%d details=%#v", closes, details)
			}
			if !tc.compatible {
				if h.clientError != legacyMessage {
					t.Fatalf("unrelated close changed: got %q, want %q", h.clientError, legacyMessage)
				}
				return
			}
			var body struct {
				Error struct{ Type, Code, Message string }
			}
			if err := json.Unmarshal([]byte(h.clientError), &body); err != nil {
				t.Fatalf("host close did not receive a complete JSON error: %q (%v)", h.clientError, err)
			}
			var upstream *upstreamError
			if !errors.As(tc.err, &upstream) {
				t.Fatalf("expected a classified upstream fault: %v", tc.err)
			}
			marker := "[clinepassbridge:cpa_compat:server misbehaving]"
			if upstream.Kind == upstreamNetworkKind {
				marker = "[clinepassbridge:cpa_compat:connection aborted]"
			}
			if body.Error.Type != "server_error" || body.Error.Code != "500" || body.Error.Message != legacyMessage || !strings.Contains(body.Error.Message, marker) || strings.ContainsAny(h.clientError, "\r\n") {
				t.Fatalf("client retry classification or single-line framing lost: %q", h.clientError)
			}
			if details.Code != upstream.Kind || details.Scope != "request" || !details.RequestScoped || details.RetryAt != nil {
				t.Fatalf("client compatibility changed host error details: %#v", details)
			}
			if message, status := s.HostErrorDetails(tc.err); message != rpcMessage || status != rpcStatus || status != 0 || json.Valid([]byte(message)) {
				t.Fatalf("stream compatibility changed the RPC boundary: status=%d message=%q", status, message)
			}
		})
	}
}

func TestStreamNetworkRetryCompatibilityRequiresTypedEOF(t *testing.T) {
	for _, kind := range []string{"", "connection_reset", "connection_closed", "timeout", "dns", "tls", "network"} {
		t.Run("transport="+kind, func(t *testing.T) {
			s := NewService()
			// Neither an EOF-looking message nor another network subtype grants
			// retry compatibility: the constructor must preserve typed identity.
			err := &upstreamError{
				APIError:      &APIError{Status: 502, Kind: upstreamNetworkKind, Message: "upstream response interrupted (eof): unexpected EOF"},
				Scope:         "request",
				TransportKind: kind,
			}
			h := newFakeHost()
			s.SetHost(h.call)
			s.closeOutputStream("fixture", "/v1/chat/completions", &LogEntry{ErrorPhase: "after_output"}, err)
			legacy, _ := s.hostErrorDetails(err, true)
			if h.clientError != legacy || json.Valid([]byte(h.clientError)) {
				t.Fatalf("message text or non-EOF transport acquired retry JSON: %q", h.clientError)
			}
		})
	}
}

type retryEOFBody struct {
	payload []byte
	closed  atomic.Int32
}

func (body *retryEOFBody) Read(buffer []byte) (int, error) {
	n := copy(buffer, body.payload)
	body.payload = body.payload[n:]
	if len(body.payload) == 0 {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (body *retryEOFBody) Close() error {
	body.closed.Add(1)
	return nil
}

func TestExecuteStreamEgressEOFPreservesDiagnosticsAndOnlyClosesOnce(t *testing.T) {
	s := egressTestService(t, directTestEndpoint("fixture", "fixture", 60, 2))
	body := &retryEOFBody{payload: idleDeltaFrame(map[string]any{"reasoning_content": "partial reasoning"})}
	var sends atomic.Int32
	p := s.getEgressManager()
	p.client("direct").Transport = egressPolicyRoundTripper(func(r *http.Request) (*http.Response, error) {
		sends.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: body, Request: r}, nil
	})
	h := newFakeHost()
	closes := 0
	var details streamErrorDetails
	s.SetHost(func(method string, payload, out any) error {
		if method != "host.stream.emit" && method != "host.stream.close" {
			t.Fatalf("egress request unexpectedly used host callback %s", method)
		}
		if method == "host.stream.close" {
			closes++
			details = payload.(map[string]any)["error_details"].(streamErrorDetails)
		}
		return h.call(method, payload, out)
	})
	var request ExecutorRequest
	if err := json.Unmarshal(executorRequest("client-eof"), &request); err != nil {
		t.Fatal(err)
	}
	request.Metadata = map[string]any{"request_path": "/v1/chat/completions"}
	if _, err := s.Handle("executor.execute_stream", jsonBytes(request)); err != nil {
		t.Fatalf("stream handoff failed: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("unexpected EOF did not terminate the output stream")
	}
	s.active.Wait()
	if closes != 1 || sends.Load() != 1 || len(h.emitted) != 1 || body.closed.Load() != 1 {
		t.Fatalf("EOF was replayed or closed more than once: sends=%d emits=%d closes=%d body_closes=%d", sends.Load(), len(h.emitted), closes, body.closed.Load())
	}
	var frame struct {
		Choices []struct {
			Delta struct {
				Reasoning string `json:"reasoning_content"`
				Content   string `json:"content"`
			}
		}
	}
	if err := json.Unmarshal(h.emitted[0], &frame); err != nil || len(frame.Choices) != 1 || frame.Choices[0].Delta.Reasoning != "partial reasoning" || frame.Choices[0].Delta.Content != "" {
		t.Fatalf("partial reasoning changed or acquired terminal data: %s (%v)", h.emitted[0], err)
	}
	var wire struct {
		Error struct{ Type, Code, Message string }
	}
	if err := json.Unmarshal([]byte(h.clientError), &wire); err != nil || wire.Error.Type != "server_error" || wire.Error.Code != "500" || wire.Error.Message != "HTTP 502: upstream connection interrupted (upstream_network_error): [clinepassbridge:cpa_compat:connection aborted]" {
		t.Fatalf("host did not receive the EOF retry error: %q (%v)", h.clientError, err)
	}
	if details.HTTPStatus != 502 || details.Code != upstreamNetworkKind || details.Scope != "request" || !details.RequestScoped || details.Phase != "after_output" || details.RetryAt != nil {
		t.Fatalf("EOF compatibility changed error_details: %#v", details)
	}
	if len(s.logs) != 1 {
		t.Fatalf("request logs=%d", len(s.logs))
	}
	entry := s.logs[0]
	const original = "HTTP 502: egress upstream response interrupted (eof)"
	if entry.Error != original || entry.ErrorKind != upstreamNetworkKind || entry.Status != 502 || entry.RateLimitScope != "request" || entry.ErrorPhase != "after_output" || entry.DownstreamStarted == nil || !*entry.DownstreamStarted || entry.EffectiveFrameCount != 1 || entry.UpstreamHTTPStatus != 0 || entry.UpstreamErrorStatus != 0 {
		t.Fatalf("EOF raw error, scope or existing status semantics changed: %#v", entry)
	}
	if entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 1 || len(entry.Attempts) != 1 || entry.Attempts[0].Error != original || entry.Attempts[0].ErrorKind != upstreamNetworkKind || entry.CooldownDisposition != "legacy_host_unknown" {
		t.Fatalf("EOF compatibility changed attempts or host receipt: %#v", entry)
	}
	for _, data := range [][]byte{jsonBytes(details), jsonBytes(entry)} {
		if strings.Contains(string(data), "TransportKind") || strings.Contains(string(data), "transport_kind") {
			t.Fatalf("internal EOF identity leaked into wire or request logs: %s", data)
		}
	}
}

func TestExecuteStreamServiceErrorPreservesDiagnosticsAndOnlyClosesOnce(t *testing.T) {
	const original = "Stream error occurred"
	s := registeredService(t, "stream-aggregate")
	h := newFakeHost(ssePlan(
		idleDeltaFrame(map[string]any{"content": "partial response"}),
		sseFrame(map[string]any{"error": original}),
	))
	closes := 0
	var details streamErrorDetails
	s.SetHost(func(method string, payload, out any) error {
		if method == "host.stream.close" {
			closes++
			details = payload.(map[string]any)["error_details"].(streamErrorDetails)
		}
		return h.call(method, payload, out)
	})
	var request ExecutorRequest
	if err := json.Unmarshal(executorRequest("client-1"), &request); err != nil {
		t.Fatal(err)
	}
	request.Metadata = map[string]any{"request_path": "/v1/chat/completions"}
	if _, err := s.Handle("executor.execute_stream", jsonBytes(request)); err != nil {
		t.Fatalf("stream handoff failed: %v", err)
	}
	select {
	case <-h.clientClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("failed upstream did not close the output stream")
	}
	s.active.Wait()
	if closes != 1 || len(h.opened) != 1 || len(h.emitted) != 1 {
		t.Fatalf("terminal failure was replayed or emitted as extra data: closes=%d requests=%d chunks=%d", closes, len(h.opened), len(h.emitted))
	}
	var frame struct {
		Choices []struct{ Delta struct{ Content string } }
	}
	if err := json.Unmarshal(h.emitted[0], &frame); err != nil || len(frame.Choices) != 1 || frame.Choices[0].Delta.Content != "partial response" {
		t.Fatalf("delivered content changed: %s (%v)", h.emitted[0], err)
	}
	var body struct {
		Error struct{ Type, Code, Message string }
	}
	if err := json.Unmarshal([]byte(h.clientError), &body); err != nil || body.Error.Type != "server_error" || body.Error.Code != "500" || !strings.Contains(body.Error.Message, "upstream_service_error") {
		t.Fatalf("request path did not reach the terminal compatibility encoder: %q (%v)", h.clientError, err)
	}
	if details.HTTPStatus != 502 || details.Code != upstreamServiceKind || details.Scope != "request" || !details.RequestScoped || details.Phase != "after_output" {
		t.Fatalf("terminal host diagnostics changed: %#v", details)
	}
	if len(s.logs) != 1 {
		t.Fatalf("terminal logs=%d", len(s.logs))
	}
	entry := s.logs[0]
	if entry.Error != original || entry.Status != 502 || entry.ErrorKind != upstreamServiceKind || entry.UpstreamHTTPStatus != 200 || entry.UpstreamErrorStatus != 502 || entry.ErrorPhase != "after_output" || entry.DownstreamStarted == nil || !*entry.DownstreamStarted || entry.EffectiveFrameCount != 1 {
		t.Fatalf("original upstream error or output phase lost: %#v", entry)
	}
	if entry.UpstreamAttempts == nil || *entry.UpstreamAttempts != 1 || len(entry.Attempts) != 1 || entry.Attempts[0].Error != original || entry.Attempts[0].ErrorKind != upstreamServiceKind || entry.CooldownDisposition != "legacy_host_unknown" {
		t.Fatalf("client compatibility changed attempts or host receipt: %#v", entry)
	}
}
