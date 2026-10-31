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
	Port string
	Bond *BondInput
}

type BondInput struct {
	Name    string
	Members []string
}

type GroupInput struct {
	Name  string
	Ports []DirectedPortInput
}

type DirectedPortInput struct {
	Interface string
	Direction Direction
	Bond      *BondInput
}
