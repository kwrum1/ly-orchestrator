package orchestrator

const SchemaVersion = 1

type TopologyInput struct {
	SchemaVersion       int
	ManagementInterface string
	ManagementShared    bool
	Interfaces          []InterfaceInput
	Groups              []GroupInput
	// PortRoles is the operator's LAN/WAN tag per physical port. It is
	// persisted with the topology and echoed by the API; it never changes the
	// logical LAN/WAN port ownership by itself. The console uses it to point
	// orchestration-group port selectors at the ports that actually face LAN
	// or WAN, which is what the frozen UI's 网卡设置 → 角色配置 wrote.
	PortRoles map[string]Role
}

type InterfaceInput struct {
	Name string
	Role Role
	// Port is the physical data port for a plain LAN/WAN boundary.
	Port string
	// Bond replaces Port for an aggregated LAN/WAN (ingress/egress) boundary.
	// The aggregate is created as lyroute-bond-<name>; members are the
	// physical data ports that carry the boundary traffic.
	Bond *BondInput
}

type BondInput struct {
	Name    string
	Members []string
}

type GroupInput struct {
	Name string
	// Ports is exactly one LAN-facing and one WAN-facing port. Either side may
	// be a physical interface or an inline aggregate bond.
	Ports []DirectedPortInput
}

type DirectedPortInput struct {
	Interface string
	Direction Direction
	// Bond declares an aggregated group port inline (bond:{name,members}).
	// The aggregate is created before the plugin apply and is addressed as
	// lyroute-bond-<name>. It must declare its own name/members; a group port
	// may not reuse the logical LAN/WAN bond.
	Bond *BondInput
}
