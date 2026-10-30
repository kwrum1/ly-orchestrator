package vpp

// LiveInterfaceNames parses `show interface` output into the set of interface
// names. It is exported so the control-plane planner can resolve a live
// proxy-service TAP without reaching into the VPP execution internals.
func LiveInterfaceNames(output string) map[string]bool {
	return parseVPPInterfaceNames(output)
}
