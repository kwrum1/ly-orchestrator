package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ly-route/backend/internal/orchestratorapi"
	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
	"ly-route/backend/internal/productupdate"
)

func waitForUpgradeState(t *testing.T, server *Server, cookie *http.Cookie, taskID, want string) *httptest.ResponseRecorder {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last *httptest.ResponseRecorder
	for time.Now().Before(deadline) {
		last = authenticatedJSONRequest(t, server, http.MethodGet, orchestratorapi.UpgradesPath+"/"+taskID, "", cookie)
		if last.Code == http.StatusOK {
			var body struct {
				Item orchestratorapi.UpgradeTask `json:"item"`
			}
			if json.Unmarshal(last.Body.Bytes(), &body) == nil && body.Item.State == want {
				return last
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return last
}

func TestOrchestratorUpgradeAcceptsBeforeStartingInstaller(t *testing.T) {
	server, _, cookie := orchestratorPackageServer(t)
	server.orchestratorUpgradeDir = t.TempDir()
	started := make(chan struct{})
	release := make(chan struct{})
	server.orchestratorInstallStart = func(invocation orchestratorUpgradeInvocation) error {
		close(started)
		<-release
		resultPath := invocation.Args[8]
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o750); err != nil {
			return err
		}
		return os.WriteFile(resultPath, []byte(`{"state":"installed","reason":"test"}`), 0o600)
	}
	pkg := testUpgradePackage(t, product.Orchestrator().ID(), false)
	staged := authenticatedMultipartRequest(t, server, orchestratorapi.UpgradesPath, map[string][]byte{"package": pkg}, cookie)
	var body struct {
		Item orchestratorapi.UpgradeTask `json:"item"`
	}
	decode(t, staged, &body)

	response := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.UpgradesPath+"/"+body.Item.ID+"/install", `{"confirm_install":true}`, cookie)
	if response.Code != http.StatusAccepted {
		t.Fatalf("install status = %d: %s", response.Code, response.Body.String())
	}
	if response.Header().Get("Location") != orchestratorapi.UpgradesPath+"/"+body.Item.ID {
		t.Fatalf("Location = %q", response.Header().Get("Location"))
	}
	if !containsAll(response.Body.String(), `"task_id":"`+body.Item.ID+`"`, `"status":"installing"`, `"poll":"`) {
		t.Fatalf("accepted body = %s", response.Body.String())
	}
	select {
	case <-started:
		t.Fatal("installer started before the 202 response was observed")
	default:
	}
	close(release)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("installer did not start asynchronously")
	}
	waitForUpgradeState(t, server, cookie, body.Item.ID, orchestratorapi.UpgradeStateInstalled)
}

func TestOrchestratorUpgradeTaskLifecyclePersistsAndRollsBack(t *testing.T) {
	server, _, cookie := orchestratorPackageServer(t)
	server.orchestratorUpgradeDir = t.TempDir()
	var invocations []orchestratorUpgradeInvocation
	server.orchestratorInstallStart = func(invocation orchestratorUpgradeInvocation) error {
		invocations = append(invocations, invocation)
		action := invocation.Args[3]
		resultPath := invocation.Args[8]
		state := "installed"
		if action == "rollback" {
			state = "rolled_back"
		}
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o750); err != nil {
			return err
		}
		return os.WriteFile(resultPath, []byte(`{"state":"`+state+`","reason":"test"}`), 0o600)
	}

	pkg := testUpgradePackage(t, product.Orchestrator().ID(), false)
	staged := authenticatedMultipartRequest(t, server, orchestratorapi.UpgradesPath, map[string][]byte{"package": pkg}, cookie)
	if staged.Code != http.StatusCreated {
		t.Fatalf("stage status = %d: %s", staged.Code, staged.Body.String())
	}
	var stagedBody struct {
		Item orchestratorapi.UpgradeTask `json:"item"`
	}
	decode(t, staged, &stagedBody)
	if stagedBody.Item.State != orchestratorapi.UpgradeStateStaged || stagedBody.Item.PackageSize == 0 {
		t.Fatalf("staged task = %#v", stagedBody.Item)
	}

	listed := authenticatedJSONRequest(t, server, http.MethodGet, orchestratorapi.UpgradesPath, "", cookie)
	if listed.Code != http.StatusOK || !containsAll(listed.Body.String(), `"upgrade_supported":true`, stagedBody.Item.ID) {
		t.Fatalf("list upgrades status=%d body=%s", listed.Code, listed.Body.String())
	}

	installed := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.UpgradesPath+"/"+stagedBody.Item.ID+"/install", `{"confirm_install":true}`, cookie)
	if installed.Code != http.StatusAccepted {
		t.Fatalf("install status = %d: %s", installed.Code, installed.Body.String())
	}
	var installedBody struct {
		Item orchestratorapi.UpgradeTask `json:"item"`
	}
	decode(t, installed, &installedBody)
	if installedBody.Item.State != orchestratorapi.UpgradeStateInstalling {
		t.Fatalf("install task state = %q", installedBody.Item.State)
	}
	readback := waitForUpgradeState(t, server, cookie, stagedBody.Item.ID, orchestratorapi.UpgradeStateInstalled)
	if readback.Code != http.StatusOK {
		t.Fatalf("install readback status=%d body=%s", readback.Code, readback.Body.String())
	}
	if len(invocations) != 1 || invocations[0].Args[3] != "install" {
		t.Fatalf("install invocations = %#v", invocations)
	}

	rolled := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.UpgradesPath+"/"+stagedBody.Item.ID+"/rollback", `{}`, cookie)
	if rolled.Code != http.StatusAccepted {
		t.Fatalf("rollback status = %d: %s", rolled.Code, rolled.Body.String())
	}
	rollbackReadback := waitForUpgradeState(t, server, cookie, stagedBody.Item.ID, orchestratorapi.UpgradeStateRolledBack)
	if rollbackReadback.Code != http.StatusOK {
		t.Fatalf("rollback readback status=%d body=%s", rollbackReadback.Code, rollbackReadback.Body.String())
	}
	if len(invocations) != 2 || invocations[1].Args[3] != "rollback" {
		t.Fatalf("rollback invocations = %#v", invocations)
	}
	events, err := server.store.AuditEvents(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 {
		t.Fatalf("upgrade audit events = %#v", events)
	}
}

func TestOrchestratorUpgradeRejectsWrongProductAndMissingConfirmation(t *testing.T) {
	server, _, cookie := orchestratorPackageServer(t)
	server.orchestratorUpgradeDir = t.TempDir()
	wrong := testUpgradePackage(t, product.Gateway().ID(), false)
	response := authenticatedMultipartRequest(t, server, orchestratorapi.UpgradesPath, map[string][]byte{"package": wrong}, cookie)
	if response.Code != http.StatusUnprocessableEntity || !containsAll(response.Body.String(), "product") {
		t.Fatalf("wrong product status=%d body=%s", response.Code, response.Body.String())
	}
	pkg := testUpgradePackage(t, product.Orchestrator().ID(), false)
	staged := authenticatedMultipartRequest(t, server, orchestratorapi.UpgradesPath, map[string][]byte{"package": pkg}, cookie)
	var body struct {
		Item orchestratorapi.UpgradeTask `json:"item"`
	}
	decode(t, staged, &body)
	missing := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.UpgradesPath+"/"+body.Item.ID+"/install", `{}`, cookie)
	if missing.Code == http.StatusAccepted {
		t.Fatalf("install without confirmation was accepted: %s", missing.Body.String())
	}
}

func TestOrchestratorUpgradeInstallConflictWhileInstalling(t *testing.T) {
	server, _, cookie := orchestratorPackageServer(t)
	server.orchestratorUpgradeDir = t.TempDir()
	server.orchestratorInstallStart = func(orchestratorUpgradeInvocation) error { return nil }
	pkg := testUpgradePackage(t, product.Orchestrator().ID(), false)
	staged := authenticatedMultipartRequest(t, server, orchestratorapi.UpgradesPath, map[string][]byte{"package": pkg}, cookie)
	var body struct {
		Item orchestratorapi.UpgradeTask `json:"item"`
	}
	decode(t, staged, &body)
	first := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.UpgradesPath+"/"+body.Item.ID+"/install", `{"confirm_install":true}`, cookie)
	if first.Code != http.StatusAccepted {
		t.Fatalf("first install status = %d: %s", first.Code, first.Body.String())
	}
	second := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.UpgradesPath+"/"+body.Item.ID+"/install", `{"confirm_install":true}`, cookie)
	if second.Code != http.StatusConflict {
		t.Fatalf("second install status = %d, want conflict: %s", second.Code, second.Body.String())
	}
}

func TestUpgradeInstallerScriptsParse(t *testing.T) {
	for name, script := range map[string]string{
		"gateway":      firmwareUpgradeInstallScript,
		"orchestrator": orchestratorUpgradeScript,
	} {
		command := exec.Command("bash", "-n")
		command.Stdin = strings.NewReader(script)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("%s installer script syntax: %v: %s", name, err, strings.TrimSpace(string(output)))
		}
	}
}

func TestOrchestratorUpgradeUnitArgvMaterializesMultilineScript(t *testing.T) {
	invocation := orchestratorUpgradeScriptInvocation("install", "/tmp/pkg", "hash", "/usr/lib/ly-route", "/tmp/backup", "/tmp/result", false)
	invocation.Unit = "ly-route-orch-upgrade-test-argv"
	argv, err := orchestratorUpgradeUnitArgv(invocation)
	if err != nil {
		t.Fatal(err)
	}
	if len(argv) < 3 || argv[0] != "bash" || argv[2] != "install" {
		t.Fatalf("detached argv = %#v", argv)
	}
	body, err := os.ReadFile(argv[1])
	if err != nil {
		t.Fatalf("read materialized script: %v", err)
	}
	if !strings.Contains(string(body), "set -eu") || strings.Contains(strings.Join(argv, " "), "\n") {
		t.Fatalf("detached script argv is not safe: %#v", argv)
	}
	unitArgs, err := orchestratorUpgradeSystemdRunArgs(invocation)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(unitArgs, "--no-block") {
		t.Fatalf("systemd-run args missing --no-block: %#v", unitArgs)
	}
}

func TestOrchestratorUpgradeUnitNameIsStableAndSafe(t *testing.T) {
	if got := orchestratorUpgradeUnitName("upgrade-ABC_123.4"); got != "ly-route-orch-upgrade-upgrade-abc-123-4" {
		t.Fatalf("unit name = %q", got)
	}
	if got := orchestratorUpgradeUnitName("  "); got != "ly-route-orch-upgrade-unknown" {
		t.Fatalf("empty unit name = %q", got)
	}
	invocation := orchestratorUpgradeScriptInvocation("install", "/tmp/pkg", "hash", "/usr/lib/ly-route", "/tmp/backup", "/tmp/result", false)
	if invocation.Args[3] != "install" || invocation.Args[8] != "/tmp/result" {
		t.Fatalf("invocation args = %#v", invocation.Args)
	}
}

func TestOrchestratorUpgradeInstallerHasBackupRollbackAndServiceState(t *testing.T) {
	script := orchestratorUpgradeScript
	for _, required := range []string{
		"backup_dir/services.state",
		"restore_services",
		"capture_services",
		"ly_route_orchestrator_plugin.so",
		"ly-route-control-api.service",
		"version.env",
		"timeout 30 systemctl restart ly-route-control-api.service",
		"write_result failed \"install failed; restoring backup\"",
		"health_check",
		"systemctl reboot",
	} {
		if !strings.Contains(script, required) {
			t.Fatalf("orchestrator installer is missing %q", required)
		}
	}
	if strings.Contains(script, "reboot -f") {
		t.Fatal("orchestrator installer forces an unconditional reboot")
	}
	if strings.Contains(script, "smartdns") || strings.Contains(script, "kea-dhcp4") {
		t.Fatal("orchestrator installer touches Gateway-only services")
	}
}

func TestGatewayConversionPrepareAndRollbackRestoresServiceState(t *testing.T) {
	server, _, cookie := productTestServer(t, product.Gateway())
	state := &fakeConversionServiceState{state: map[string]string{"vpp.service": "enabled", "nginx.service": "disabled"}}
	server.conversionServiceState = state

	payload := conversionJSON(t, validConversionManifestForTest(), true)
	created := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.ConversionsPath, payload, cookie)
	if created.Code != http.StatusCreated {
		t.Fatalf("conversion create status=%d body=%s", created.Code, created.Body.String())
	}
	var body struct {
		Item orchestratorapi.ConversionTask `json:"item"`
	}
	decode(t, created, &body)
	if body.Item.State != orchestratorapi.ConversionStateReady || body.Item.SnapshotID == "" || body.Item.Plan.DataplaneMode != "preserve" {
		t.Fatalf("prepared conversion = %#v", body.Item)
	}
	if len(state.captured) == 0 {
		t.Fatalf("service state was not captured: %#v", state)
	}

	rolled := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.ConversionsPath+"/"+body.Item.ID+"/rollback", `{}`, cookie)
	if rolled.Code != http.StatusAccepted || !containsAll(rolled.Body.String(), `"state":"rolled_back"`) {
		t.Fatalf("conversion rollback status=%d body=%s", rolled.Code, rolled.Body.String())
	}
	if state.restored == nil {
		t.Fatal("conversion rollback did not restore service state")
	}

	blocked := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.ConversionsPath+"/"+body.Item.ID+"/apply", `{}`, cookie)
	if blocked.Code != http.StatusNotImplemented {
		t.Fatalf("conversion apply status=%d body=%s", blocked.Code, blocked.Body.String())
	}
}

func TestGatewayConversionPrepareIsSerialized(t *testing.T) {
	server, _, cookie := productTestServer(t, product.Gateway())
	server.conversionServiceState = &fakeConversionServiceState{state: map[string]string{"vpp.service": "enabled"}}
	payload := conversionJSON(t, validConversionManifestForTest(), false)
	created := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.ConversionsPath, payload, cookie)
	var body struct {
		Item orchestratorapi.ConversionTask `json:"item"`
	}
	decode(t, created, &body)
	codes := make(chan int, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			request := httptest.NewRequest(http.MethodPost, orchestratorapi.ConversionsPath+"/"+body.Item.ID+"/prepare", strings.NewReader(`{}`))
			request.Header.Set("Content-Type", "application/json")
			request.AddCookie(cookie)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			codes <- response.Code
		}()
	}
	wait.Wait()
	close(codes)
	success, conflict := 0, 0
	for code := range codes {
		switch code {
		case http.StatusAccepted:
			success++
		case http.StatusConflict:
			conflict++
		default:
			t.Fatalf("prepare status = %d", code)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("prepare results success=%d conflict=%d, want 1/1", success, conflict)
	}
}

func TestGatewayConversionValidationRejectsFakeDataplane(t *testing.T) {
	server, _, cookie := productTestServer(t, product.Gateway())
	manifest := validConversionManifestForTest()
	manifest.Dataplane = productupdate.DataplaneSpec{}
	payload := conversionJSON(t, manifest, false)
	response := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.ConversionsPath+"/validate", payload, cookie)
	if response.Code != http.StatusOK || !containsAll(response.Body.String(), `"valid":false`, "dataplane.preserve") {
		t.Fatalf("validation status=%d body=%s", response.Code, response.Body.String())
	}
	list := authenticatedJSONRequest(t, server, http.MethodGet, orchestratorapi.ConversionsPath, "", cookie)
	if list.Code != http.StatusOK || !containsAll(list.Body.String(), `"items":[]`) {
		t.Fatalf("rejected conversion created a task: %d %s", list.Code, list.Body.String())
	}
}

func TestGatewayConversionSnapshotSurvivesTaskReload(t *testing.T) {
	server, _, cookie := productTestServer(t, product.Gateway())
	server.conversionServiceState = &fakeConversionServiceState{state: map[string]string{"vpp.service": "enabled"}}
	payload := conversionJSON(t, validConversionManifestForTest(), true)
	created := authenticatedJSONRequest(t, server, http.MethodPost, orchestratorapi.ConversionsPath, payload, cookie)
	var body struct {
		Item orchestratorapi.ConversionTask `json:"item"`
	}
	decode(t, created, &body)
	records, err := server.store.ProductTasks(context.Background(), persistence.ProductTaskConversion)
	if err != nil || len(records) != 1 {
		t.Fatalf("conversion tasks = %#v err=%v", records, err)
	}
	var stored conversionTaskPayload
	if err := json.Unmarshal(records[0].Payload, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Task.SnapshotID != body.Item.SnapshotID || stored.Task.State != orchestratorapi.ConversionStateReady {
		t.Fatalf("stored conversion task = %#v", stored.Task)
	}
}

type fakeConversionServiceState struct {
	state    map[string]string
	captured map[string]string
	restored map[string]string
}

func (controller *fakeConversionServiceState) Capture(_ context.Context, units []string) (map[string]string, error) {
	controller.captured = map[string]string{}
	for _, unit := range units {
		controller.captured[unit] = controller.state[unit]
	}
	return controller.captured, nil
}

func (controller *fakeConversionServiceState) Restore(_ context.Context, state map[string]string) error {
	controller.restored = map[string]string{}
	for unit, value := range state {
		controller.restored[unit] = value
	}
	return nil
}

func validConversionManifestForTest() productupdate.Manifest {
	return productupdate.Manifest{
		SchemaVersion: productupdate.SchemaVersion,
		PackageType:   productupdate.ConversionPackageType,
		FromProduct:   product.Gateway().ID(),
		ToProduct:     product.Orchestrator().ID(),
		FromVersion:   "26.06+lyroute1",
		ToVersion:     "26.06+lyroute1",
		BaseVersion:   "26.06",
		Suite:         "bookworm",
		Arch:          "amd64",
		Dataplane: productupdate.DataplaneSpec{
			Preserve:       true,
			VerifyPackages: []string{"vpp", "vpp-plugin-core", "ly-route-vpp-orchestrator"},
			RequiredPlugin: "ly_route_orchestrator_plugin.so",
		},
		Config:    productupdate.ConfigSpec{Migration: "preserve", SnapshotRequired: true},
		Rollback:  productupdate.RollbackSpec{Supported: true, BackupRequired: true},
		Services:  productupdate.ServicesSpec{PreserveEnableState: true, Units: []string{"vpp.service", "nginx.service"}},
		Checksums: map[string]string{"usr/lib/ly-route/ly-route-control": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	}
}

func conversionJSON(t *testing.T, manifest productupdate.Manifest, prepare bool) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"manifest": manifest, "prepare": prepare})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
