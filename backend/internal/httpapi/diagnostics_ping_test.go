package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseDiagnosticsPingOutputVPPStyle(t *testing.T) {
	t.Parallel()
	result := diagnosticsPingResult{Count: 3}
	result.Output = strings.Join([]string{
		"116 bytes from 10.99.0.4: icmp_seq=2 ttl=64 time=12.4281 ms",
		"116 bytes from 10.99.0.4: icmp_seq=3 ttl=64 time=12.3587 ms",
		"",
		"Statistics: 3 sent, 2 received, 33% packet loss",
	}, "\n")
	parseDiagnosticsPingOutput(&result)
	if result.Sent != 3 || result.Received != 2 {
		t.Fatalf("sent/received = %d/%d, want 3/2", result.Sent, result.Received)
	}
	if len(result.Packets) != 3 {
		t.Fatalf("packets = %d, want 3 (one row per probe)", len(result.Packets))
	}
	if result.Packets[0].Status != "timeout" || result.Packets[1].Status != "ok" {
		t.Fatalf("unexpected packet statuses: %+v", result.Packets)
	}
	if result.MinRTTms <= 0 || result.MaxRTTms < result.MinRTTms || result.AvgRTTms <= 0 {
		t.Fatalf("unexpected rtt stats: %+v", result)
	}
	if result.LossPercent < 33 || result.LossPercent > 34 {
		t.Fatalf("loss = %v, want 33.3", result.LossPercent)
	}
}

func TestParseDiagnosticsPingOutputLinuxStyle(t *testing.T) {
	t.Parallel()
	result := diagnosticsPingResult{Count: 2}
	result.Output = strings.Join([]string{
		"PING 192.168.10.10 (192.168.10.10) from 192.168.10.1 : 56(84) bytes of data.",
		"64 bytes from 192.168.10.10: icmp_seq=1 ttl=64 time=0.412 ms",
		"64 bytes from 192.168.10.10: icmp_seq=2 ttl=64 time=0.377 ms",
		"",
		"--- 192.168.10.10 ping statistics ---",
		"2 packets transmitted, 2 received, 0% packet loss, time 1001ms",
	}, "\n")
	parseDiagnosticsPingOutput(&result)
	if result.Sent != 2 || result.Received != 2 {
		t.Fatalf("sent/received = %d/%d, want 2/2", result.Sent, result.Received)
	}
	if result.LossPercent != 0 {
		t.Fatalf("loss = %v, want 0", result.LossPercent)
	}
	if result.Packets[0].Bytes != 64 || result.Packets[0].TTL != 64 {
		t.Fatalf("unexpected first packet: %+v", result.Packets[0])
	}
}

func TestDiagnosticsPingRequestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		body    string
		wantErr string
		count   int
		timeout int
	}{
		{name: "defaults", body: `{"target":"10.99.0.4"}`, count: 4, timeout: 2000},
		{name: "missing target", body: `{}`, wantErr: "target is required"},
		{name: "count too high", body: `{"target":"10.99.0.4","count":21}`, wantErr: "count must be between"},
		{name: "timeout too low", body: `{"target":"10.99.0.4","timeout_ms":10}`, wantErr: "timeout_ms must be between"},
		{name: "bad json", body: `{`, wantErr: "valid JSON"},
	}
	for _, testCase := range cases {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/diagnostics/ping", strings.NewReader(testCase.body))
		parsed, err := diagnosticsPingRequestFromRequest(request)
		if testCase.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("%s: err = %v, want %q", testCase.name, err, testCase.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: unexpected error %v", testCase.name, err)
		}
		if parsed.Count != testCase.count || parsed.TimeoutMS != testCase.timeout {
			t.Fatalf("%s: parsed = %+v", testCase.name, parsed)
		}
	}
}

func TestDiagnosticsPingRejectsNonIPv4Literal(t *testing.T) {
	t.Parallel()
	if _, err := resolveDiagnosticsTarget(context.Background(), "not a host.."); err == nil {
		t.Fatal("expected an invalid target to be rejected")
	}
	if address, err := resolveDiagnosticsTarget(context.Background(), "10.99.0.4"); err != nil || address.String() != "10.99.0.4" {
		t.Fatalf("literal address rejected: %v %v", address, err)
	}
}
