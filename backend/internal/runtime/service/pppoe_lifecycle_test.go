package service

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFilesystemControllerStopPPPoEStopsEveryInstanceAndVerifiesInterfacesGone(t *testing.T) {
	artifacts, err := RenderPPPoEConfig([]PPPoEPeer{
		{ID: "wan-blue", Interface: "eth7", Username: "blue", Password: "secret"},
		{ID: "wan-green", Interface: "eth8", Username: "green", Password: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{outputs: map[string]string{
		"vppctl show pppoe session": "No pppoe sessions configured...",
		"vppctl show pppoe client":  "No pppoe clients configured...",
	}}
	controller := FilesystemController{Runner: runner}
	if err := controller.Stop(context.Background(), PPPd, artifacts); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(runner.commands, "\n")
	for _, required := range []string{
		"systemctl stop ly-route-pppoe@ly-route-wan-blue.service",
		"systemctl stop ly-route-pppoe@ly-route-wan-green.service",
		"systemctl stop ly-route-pppoe.target",
		"vppctl show pppoe session",
		"vppctl show pppoe client",
	} {
		if !strings.Contains(commands, required) {
			t.Fatalf("PPPoE stop commands missing %q:\n%s", required, commands)
		}
	}
	if strings.Contains(commands, "disable") {
		t.Fatalf("PPPoE stop must not disable the unit:\n%s", commands)
	}
}

func TestFilesystemControllerApplyPPPoEActivatesTargetBeforeInstances(t *testing.T) {
	artifacts, err := RenderPPPoEConfig([]PPPoEPeer{{ID: "wan-blue", Interface: "eth7", Username: "blue", Password: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	controller := FilesystemController{Runner: runner}
	if err := controller.runApplyCommand(context.Background(), PPPd, artifacts); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"systemctl start ly-route-pppoe.target",
		"systemctl enable ly-route-pppoe@ly-route-wan-blue.service",
		"systemctl restart ly-route-pppoe@ly-route-wan-blue.service",
	}
	if len(runner.commands) != len(want) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, want)
	}
	for index := range want {
		if runner.commands[index] != want[index] {
			t.Fatalf("commands = %#v, want %#v", runner.commands, want)
		}
	}
}

func TestFilesystemControllerStopPPPoEContinuesAcrossInstanceFailures(t *testing.T) {
	artifacts, err := RenderPPPoEConfig([]PPPoEPeer{
		{ID: "wan-blue", Interface: "eth7", Username: "blue", Password: "secret"},
		{ID: "wan-green", Interface: "eth8", Username: "green", Password: "secret"},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{runErrs: map[string]error{
		"systemctl stop ly-route-pppoe@ly-route-wan-blue.service": errors.New("unit refused stop"),
	}}
	err = (FilesystemController{Runner: runner}).Stop(context.Background(), PPPd, artifacts)
	if err == nil || !strings.Contains(err.Error(), "wan-blue") {
		t.Fatalf("expected explicit instance stop failure, got %v", err)
	}
	if !strings.Contains(strings.Join(runner.commands, "\n"), "systemctl stop ly-route-pppoe@ly-route-wan-green.service") {
		t.Fatalf("second PPPoE instance was not stopped after first failure: %#v", runner.commands)
	}
}

func TestRenderPPPoEConfigNormalizesEmptyLists(t *testing.T) {
	artifacts, err := RenderPPPoEConfig([]PPPoEPeer{{ID: "wan-blue", Interface: "eth7", Username: "blue", Password: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	content := artifacts[0].Content
	if strings.Contains(content, "null") {
		t.Fatalf("rendered PPPoE plan contains nullable lists:\n%s", content)
	}
	if !strings.Contains(content, `"ipv6_lan_interfaces": []`) || !strings.Contains(content, `"nat_inside_interfaces": []`) {
		t.Fatalf("rendered PPPoE plan does not use stable empty lists:\n%s", content)
	}
}

func TestFilesystemControllerApplyPPPoEUnchangedHealthyDoesNotRestart(t *testing.T) {
	artifacts, err := RenderPPPoEConfig([]PPPoEPeer{{ID: "wan-blue", Interface: "eth7", Username: "blue", Password: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	controller := FilesystemController{RootDir: root}
	if err := controller.writeArtifacts(PPPd, artifacts); err != nil {
		t.Fatal(err)
	}
	statusFile := ""
	for _, artifact := range artifacts {
		if strings.HasPrefix(artifact.Path, "/etc/ly-route/pppoe/ly-route-") {
			var plan struct {
				StatusFile string `json:"status_file"`
			}
			if err := json.Unmarshal([]byte(artifact.Content), &plan); err != nil {
				t.Fatal(err)
			}
			statusFile = plan.StatusFile
			fullPath := filepath.Join(root, strings.TrimPrefix(statusFile, "/"))
			if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(fullPath, []byte(`{"state":"connected","interface":"pppoe_session0"}`), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	runner := &fakeRunner{outputs: map[string]string{
		"cat " + statusFile: `{"state":"connected","interface":"pppoe_session0"}`,
	}}
	controller.Runner = runner
	if err := controller.ReloadOrRestart(context.Background(), PPPd, artifacts); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(runner.commands, "\n")
	for _, forbidden := range []string{"systemctl restart", "systemctl stop", "systemctl disable"} {
		if strings.Contains(commands, forbidden) {
			t.Fatalf("unchanged healthy PPPoE was bounced (%s):\n%s", forbidden, commands)
		}
	}
}

func TestRuntimeStopFailsClosedWithoutVerifiedStopController(t *testing.T) {
	err := (Runtime{Controller: &fakeController{}}).Stop(context.Background(), PPPd, []RenderedArtifact{NewArtifact(PPPd, "/etc/ly-route/pppoe/ly-route-wan.json", `{}`, "restart")})
	if err == nil || !strings.Contains(err.Error(), "does not support verified stop") {
		t.Fatalf("unsupported stop did not fail closed: %v", err)
	}
}
