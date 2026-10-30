package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"ly-route/backend/internal/runtime/proxy"
	"ly-route/backend/internal/runtime/selfheal"
)

// nftSelfHealRunner is a stateful nft fake: the live table is stale until the
// managed generation is reinstalled with `nft -f`.
type nftSelfHealRunner struct {
	mu            sync.Mutex
	healthy       bool
	missingTable  bool
	failReinstall bool
	commands      []string
	expected      string
	table         string
}

func (runner *nftSelfHealRunner) Run(_ context.Context, name string, args ...string) error {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	runner.mu.Unlock()
	switch command {
	case "nft delete table inet " + runner.table:
		return nil
	case "nft -f /etc/nftables.conf":
		if runner.failReinstall {
			return errors.New("injected nft reinstall failure")
		}
		runner.mu.Lock()
		runner.healthy = true
		runner.mu.Unlock()
		return nil
	}
	return nil
}

func (runner *nftSelfHealRunner) Output(_ context.Context, name string, args ...string) (string, error) {
	command := strings.TrimSpace(name + " " + strings.Join(args, " "))
	runner.mu.Lock()
	runner.commands = append(runner.commands, command)
	healthy := runner.healthy
	runner.mu.Unlock()
	if command != "nft list table inet "+runner.table {
		return "", fmt.Errorf("unexpected readback %q", command)
	}
	if healthy {
		return runner.expected, nil
	}
	if runner.missingTable {
		return "", fmt.Errorf("nft list table inet %s: No such file or directory", runner.table)
	}
	// The table exists but the capture rules are gone: the exact production
	// "missing rule" shape.
	return "table inet " + runner.table + " {\n  chain proxy_prerouting {\n    type filter hook prerouting priority mangle; policy accept;\n  }\n}", nil
}

func (runner *nftSelfHealRunner) Status(_ context.Context, service ServiceName) (Health, error) {
	return Health{Service: service, Available: true}, nil
}

func (runner *nftSelfHealRunner) recorded(substring string) bool {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, command := range runner.commands {
		if strings.Contains(command, substring) {
			return true
		}
	}
	return false
}

func (runner *nftSelfHealRunner) resetCommands() {
	runner.mu.Lock()
	runner.commands = nil
	runner.mu.Unlock()
}

func nftSelfHealArtifacts(t *testing.T, table string) []RenderedArtifact {
	t.Helper()
	plan := proxy.NftablesCapturePlan{
		EgressID: "edge-blue", Family: "inet", Table: table, TargetPort: 15432, Mark: "0x7",
		Chains: []proxy.NftablesChain{{Name: "proxy_prerouting", Type: "filter", Hook: "prerouting", Priority: -150, Policy: "accept"}},
		Rules: []proxy.NftablesRule{
			{Order: 1, EgressID: "edge-blue", Chain: "proxy_prerouting", Expression: "iifname \"lypxhinf9d5aa\" meta nfproto ipv4 tcp dport 53", Action: "counter return"},
			{Order: 2, EgressID: "edge-blue", Chain: "proxy_prerouting", Expression: "iifname \"lypxhinf9d5aa\" meta nfproto ipv4 udp dport 53", Action: "counter return"},
			{Order: 3, EgressID: "edge-blue", Chain: "proxy_prerouting", Expression: "iifname \"lypxhinf9d5aa\" meta nfproto ipv4 meta l4proto tcp", Action: "counter meta mark set 0x7 tproxy ip to :15432 accept"},
			{Order: 4, EgressID: "edge-blue", Chain: "proxy_prerouting", Expression: "iifname \"lypxhinf9d5aa\" meta nfproto ipv4 meta l4proto udp", Action: "counter meta mark set 0x7 tproxy ip to :15432 accept"},
		},
	}
	artifacts, err := RenderNftablesCapture(plan)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts
}

func TestNftablesMissingManagedRuleIsRebuilt(t *testing.T) {
	artifacts := nftSelfHealArtifacts(t, "ly_route_proxy_capture")
	runner := &nftSelfHealRunner{table: "ly_route_proxy_capture", expected: artifacts[0].Content}
	controller := FilesystemController{RootDir: t.TempDir(), Runner: runner}
	ctx, recorder := selfheal.WithRecorder(context.Background())

	if err := controller.ReloadOrRestart(ctx, Nftables, artifacts); err != nil {
		t.Fatalf("apply with a stale managed nftables table did not self-heal: %v", err)
	}
	if !runner.recorded("nft delete table inet ly_route_proxy_capture") {
		t.Fatalf("managed table was not deleted before reinstall:\n%v", runner.commands)
	}
	if !runner.recorded("nft -f /etc/nftables.conf") {
		t.Fatalf("expected generation was not reinstalled:\n%v", runner.commands)
	}
	if got := recorder.Items(); len(got) != 1 || got[0] != "nftables" {
		t.Fatalf("self-healed scopes = %v, want [nftables]", got)
	}

	// Idempotence: a second apply against the repaired live table must not
	// rebuild again.
	runner.resetCommands()
	if err := controller.ReloadOrRestart(ctx, Nftables, artifacts); err != nil {
		t.Fatalf("second apply failed: %v", err)
	}
	if runner.recorded("nft delete table") || runner.recorded("nft -f") {
		t.Fatalf("healthy generation was rebuilt again:\n%v", runner.commands)
	}
}

func TestNftablesMissingManagedTableIsRebuilt(t *testing.T) {
	artifacts := nftSelfHealArtifacts(t, "ly_route_proxy_capture")
	runner := &nftSelfHealRunner{table: "ly_route_proxy_capture", expected: artifacts[0].Content, missingTable: true}
	controller := FilesystemController{RootDir: t.TempDir(), Runner: runner}
	ctx, recorder := selfheal.WithRecorder(context.Background())

	if err := controller.ReloadOrRestart(ctx, Nftables, artifacts); err != nil {
		t.Fatalf("apply with a missing managed nftables table did not self-heal: %v", err)
	}
	if !runner.recorded("nft -f /etc/nftables.conf") {
		t.Fatalf("expected generation was not reinstalled:\n%v", runner.commands)
	}
	if got := recorder.Items(); len(got) != 1 || got[0] != "nftables" {
		t.Fatalf("self-healed scopes = %v, want [nftables]", got)
	}
}

func TestNftablesRebuildRefusesUnmanagedTable(t *testing.T) {
	artifacts := nftSelfHealArtifacts(t, "capture_blue")
	runner := &nftSelfHealRunner{table: "capture_blue", expected: artifacts[0].Content}
	controller := FilesystemController{RootDir: t.TempDir(), Runner: runner}

	err := controller.ReloadOrRestart(context.Background(), Nftables, artifacts)
	if err == nil {
		t.Fatal("unmanaged table drift was repaired instead of failing")
	}
	if !strings.Contains(err.Error(), "refusing to rebuild non-ly-route") {
		t.Fatalf("error = %v, want explicit unmanaged-table refusal", err)
	}
	if runner.recorded("nft delete table") {
		t.Fatalf("unmanaged table was deleted:\n%v", runner.commands)
	}
}

func TestNftablesRebuildFailureStaysFailed(t *testing.T) {
	artifacts := nftSelfHealArtifacts(t, "ly_route_proxy_capture")
	runner := &nftSelfHealRunner{table: "ly_route_proxy_capture", expected: artifacts[0].Content, failReinstall: true}
	controller := FilesystemController{RootDir: t.TempDir(), Runner: runner}

	err := controller.ReloadOrRestart(context.Background(), Nftables, artifacts)
	if err == nil {
		t.Fatal("failed rebuild was reported as success")
	}
	if !strings.Contains(err.Error(), "managed nftables rebuild failed") {
		t.Fatalf("error = %v, want actionable rebuild failure", err)
	}
}
