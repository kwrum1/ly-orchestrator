package httpapi

import "testing"

func TestChooseLiveInterfacePrefersPlannedName(t *testing.T) {
	if got, changed := chooseLiveInterface("lypxinold", []string{"lypxinnew", "lypxinold"}); got != "lypxinold" || changed {
		t.Fatalf("choose = %q changed=%v, want planned name unchanged", got, changed)
	}
}

func TestChooseLiveInterfaceUsesSingleLiveCandidate(t *testing.T) {
	if got, changed := chooseLiveInterface("lypxinold", []string{"lypxinnew"}); got != "lypxinnew" || !changed {
		t.Fatalf("choose = %q changed=%v, want live candidate", got, changed)
	}
}

func TestChooseLiveInterfaceLeavesAmbiguousTopologyUntouched(t *testing.T) {
	if got, changed := chooseLiveInterface("lypxinold", []string{"lypxinone", "lypxintwo"}); got != "lypxinold" || changed {
		t.Fatalf("choose = %q changed=%v, want planned name preserved", got, changed)
	}
	if got, changed := chooseLiveInterface("lypxinold", nil); got != "lypxinold" || changed {
		t.Fatalf("empty inventory choose = %q changed=%v", got, changed)
	}
}

func TestResolveLiveVPPInterfaceScopesByPrefix(t *testing.T) {
	names := map[string]bool{"lypxinnew": true, "lypxoutnew": true}
	if got, changed := resolveLiveVPPInterface(names, "lypxinold", proxyIngressVPPPrefix); got != "lypxinnew" || !changed {
		t.Fatalf("ingress = %q changed=%v", got, changed)
	}
	if got, changed := resolveLiveVPPInterface(names, "lypxoutold", proxyEgressVPPPrefix); got != "lypxoutnew" || !changed {
		t.Fatalf("egress = %q changed=%v", got, changed)
	}
}
