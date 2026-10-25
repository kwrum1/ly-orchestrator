package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSingleNodeProxyDataPathSurvivesUnreachableExternalProbe is the data-path
// regression for the Gate A proxy fixture. It starts a real Xray node and a
// gateway Xray instance whose observatory probe URL is deliberately
// unreachable, waits past the probe interval, then fetches a token through the
// gateway. A single-outbound egress must keep forwarding: only the node's real
// destination matters, not whether the node can reach an external health URL.
func TestSingleNodeProxyDataPathSurvivesUnreachableExternalProbe(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv("LY_ROUTE_XRAY_BINARY"))
	if binary == "" {
		t.Skip("LY_ROUTE_XRAY_BINARY is not set")
	}
	dir := t.TempDir()
	token := "LYACC-PROXY-REGRESSION"
	targetListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	targetPort := targetListener.Addr().(*net.TCPAddr).Port
	target := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/proxy-token/"+token {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "proxy-ok:"+token)
	})}
	go func() { _ = target.Serve(targetListener) }()
	defer target.Close()

	nodePort := freeProxyTestPort(t)
	gatewayPort := freeProxyTestPort(t)
	nodeAccess := filepath.Join(dir, "node-access.log")
	nodeError := filepath.Join(dir, "node-error.log")
	nodeConfig := map[string]any{
		"log": map[string]any{"loglevel": "warning", "access": nodeAccess, "error": nodeError},
		"inbounds": []any{map[string]any{
			"tag": "node-in", "listen": "127.0.0.1", "port": nodePort, "protocol": "vless",
			"settings":       map[string]any{"clients": []any{map[string]any{"id": "11111111-1111-1111-1111-111111111111"}}, "decryption": "none"},
			"streamSettings": map[string]any{"network": "tcp"},
		}},
		"outbounds": []any{map[string]any{
			"tag": "direct", "protocol": "freedom",
			// The acceptance node config allows the fixture's reserved target;
			// without this Xray's built-in dialer blocks loopback addresses.
			"settings": map[string]any{"domainStrategy": "UseIP", "finalRules": []any{map[string]any{"action": "allow"}}},
		}},
	}
	nodePath := filepath.Join(dir, "node.json")
	writeProxyTestConfig(t, nodePath, nodeConfig)
	node := startProxyTestProcess(t, binary, "run", "-config", nodePath, nodeError)
	defer stopProxyTestProcess(node)
	waitProxyTCP(t, nodePort, "node")

	uuid := "11111111-1111-1111-1111-111111111111"
	nodeOutbound := XrayOutbound{
		Tag: "node-regression", Protocol: "vless",
		Settings:       map[string]any{"vnext": []any{map[string]any{"address": "127.0.0.1", "port": nodePort, "users": []any{map[string]any{"id": uuid, "encryption": "none"}}}}},
		StreamSettings: map[string]any{"network": "tcp"},
	}
	gatewayConfig := XrayConfigPayload{
		Log: XrayLog{Level: "warning"},
		Inbounds: []XrayInbound{{
			Tag: "proxy-in", Listen: "127.0.0.1", Port: gatewayPort, Protocol: "dokodemo-door",
			Settings: XrayDokodemoSettings{Address: "127.0.0.1", Port: targetPort, Network: "tcp"},
		}},
		Outbounds: []XrayOutbound{nodeOutbound},
		// Simulate the acceptance lab: the node can forward the client's
		// destination but cannot resolve/reach the external probe host.
		Observatory: &XrayObservatory{
			SubjectSelector: []string{nodeOutbound.Tag},
			ProbeURL:        "http://127.0.0.1:1/unreachable",
			ProbeInterval:   "1s",
		},
	}
	if err := EnsureFailureFallback(&gatewayConfig, "http://127.0.0.1:1/unreachable"); err != nil {
		t.Fatal(err)
	}
	if len(gatewayConfig.Routing.Balancers) != 0 || gatewayConfig.Observatory != nil {
		t.Fatalf("single-node data path is still health-gated: balancers=%#v observatory=%#v", gatewayConfig.Routing.Balancers, gatewayConfig.Observatory)
	}
	gatewayPath := filepath.Join(dir, "gateway.json")
	encoded, err := json.Marshal(gatewayConfig)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatewayPath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	gatewayErr := filepath.Join(dir, "gateway-error.log")
	gateway := startProxyTestProcess(t, binary, "run", "-config", gatewayPath, gatewayErr)
	defer stopProxyTestProcess(gateway)
	waitProxyTCP(t, gatewayPort, "gateway")

	// A health-gated config would have observed several failed probes by now
	// and blackholed the balancer.
	time.Sleep(3 * time.Second)
	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/proxy-token/%s", gatewayPort, token))
	if err != nil {
		t.Fatalf("client fetch through proxy failed: %v\nnode: %s\ngateway: %s\nconfig: %s", err, readProxyTestLog(nodeError), readProxyTestLog(gatewayErr), encoded)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), token) {
		t.Fatalf("client fetch through proxy = HTTP %d body %q", response.StatusCode, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		access, _ := os.ReadFile(nodeAccess)
		if strings.Contains(string(access), fmt.Sprintf("tcp:127.0.0.1:%d", targetPort)) && strings.Contains(string(access), "node-in >> direct") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("node access log never recorded the client destination %d: %s", targetPort, access)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func freeProxyTestPort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func writeProxyTestConfig(t *testing.T, path string, config map[string]any) {
	t.Helper()
	payload, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

func startProxyTestProcess(t *testing.T, binary string, args ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(binary, args...)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	return command
}

func stopProxyTestProcess(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = command.Process.Kill()
	_, _ = command.Process.Wait()
}

func waitProxyTCP(t *testing.T, port int, name string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		connection, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not listen on 127.0.0.1:%d: %v", name, port, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func readProxyTestLog(path string) string {
	payload, _ := os.ReadFile(path)
	return string(payload)
}
