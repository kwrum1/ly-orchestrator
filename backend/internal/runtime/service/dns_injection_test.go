package service

import (
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/dns"
)

func TestRenderSmartDNSRejectsUnsafeRuleIDBeforeWritingComment(t *testing.T) {
	plan := SmartDNSPlan{ID: "injection", Render: dns.SmartDNSRender{
		Engine:             "smartdns",
		DecisionPrecedence: dns.DNSPolicyPrecedence,
		Rules: []dns.SmartDNSRule{{
			Order:        1,
			RuleID:       "legit\naddress /injected.example/203.0.113.9",
			Domains:      []string{"media.example"},
			Action:       "empty_answer",
			OutcomeKind:  dns.ResolverOutcomeReject,
			ResolverPath: "reject_response",
		}},
		Miss: dns.SmartDNSRule{
			Order:        0,
			RuleID:       "miss",
			Action:       "empty_answer",
			OutcomeKind:  dns.ResolverOutcomeReject,
			ResolverPath: "reject_response",
		},
	}}
	artifacts, err := RenderSmartDNS(plan)
	if err == nil {
		t.Fatalf("RenderSmartDNS accepted a newline rule id: %#v", artifacts)
	}
	if !strings.Contains(err.Error(), "unsafe") {
		t.Fatalf("error = %v, want an unsafe-rule-id rejection", err)
	}
	for _, artifact := range artifacts {
		if strings.Contains(artifact.Content, "address /injected.example/203.0.113.9") {
			t.Fatalf("unsafe rule id reached generated SmartDNS config:\n%s", artifact.Content)
		}
	}
}
