package apply

import "context"

// ApplyHooks integrate the host services that are part of the same generation
// as the VPP graph.  They are supplied per transaction so the long-lived
// gateway transaction does not keep request-scoped artifacts or sessions.
type ApplyHooks struct {
	// EnsurePPPoE writes/starts the PPPoE helper and waits for a published
	// status contract (connecting is sufficient; connected is not required).
	EnsurePPPoE func(context.Context, Plan) error
	// VerifyPPPoE is the edge predicate used before dependent network nodes.
	VerifyPPPoE func(context.Context, Plan) error
	// RollbackPPPoE stops/restores the helper when the generation rolls back.
	RollbackPPPoE func(context.Context, Plan) error
	// ApplyServices applies Dnsmasq/Linux routing/SmartDNS/Xray/Nftables/IPv6RA.
	ApplyServices func(context.Context, Plan) error
	// RollbackServices restores the prior service artifacts and daemons.
	RollbackServices func(context.Context, Plan) error
	// VerifyServiceNetworks checks DNS service interfaces and their NAT44
	// input role before routes are allowed to depend on them.
	VerifyServiceNetworks func(context.Context, Plan) error
	// VerifyNAT44 checks that NAT44 is initialized before port mappings.
	VerifyNAT44 func(context.Context, Plan) error
}
