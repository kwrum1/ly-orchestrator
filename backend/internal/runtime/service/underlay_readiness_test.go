package service

import (
	"strings"
	"testing"
)

func TestRenderVPPUnderlayReadinessUsesInterfaceInventoryLookup(t *testing.T) {
	rendered, err := renderVPPUnderlayReadiness([]string{"pppoe-runtime:wan-primary"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, `show interface address "$underlay_if"`) {
		t.Fatalf("readiness probe passed an interface argument to VPP:\n%s", rendered)
	}
	for _, required := range []string{
		`pppoe-runtime:*)`,
		`show interface 2>/dev/null | awk 'NR > 1 {print $1}' | grep -Fxq "$underlay_if"`,
		`show interface address 2>/dev/null | tr -d '\r' | grep -A1 -E`,
		`grep -Eq '^  L3 [0-9]'`,
		`LY_ROUTE_VPP_UNDERLAY_READY_ATTEMPTS:-10`,
	} {
		if !strings.Contains(rendered, required) {
			t.Fatalf("readiness probe missing %q:\n%s", required, rendered)
		}
	}
}

func TestRenderVPPUnderlayReadinessCanDeferInsteadOfFailingApply(t *testing.T) {
	rendered, err := renderVPPUnderlayReadinessMode([]string{"pppoe-runtime:wan-primary"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(rendered, "exit 1") {
		t.Fatalf("deferred readiness must not abort the apply artifact:\n%s", rendered)
	}
	if !strings.Contains(rendered, "deferring dependent policy routing") {
		t.Fatalf("deferred readiness has no diagnostic:\n%s", rendered)
	}
}
