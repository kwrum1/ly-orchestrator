package vppfake

import (
	"fmt"
	"strconv"
	"strings"
)

func (state *State) show(fields []string) string {
	if len(fields) < 2 {
		return ""
	}
	switch fields[1] {
	case "interface":
		if len(fields) >= 3 && fields[2] == "address" {
			only := ""
			if len(fields) >= 4 {
				only = fields[3]
			}
			return state.showInterfaceAddress(only)
		}
		if len(fields) >= 3 {
			return state.showInterface(fields[2])
		}
		return state.showInterfaces()
	case "hardware-interfaces":
		if len(fields) >= 3 {
			return state.showHardwareInterface(fields[2])
		}
	case "lcp":
		return state.showLCP()
	case "bond":
		if len(fields) >= 3 && fields[2] == "details" {
			return state.showBonds()
		}
	case "ip":
		if len(fields) >= 3 && fields[2] == "fib" {
			if len(fields) >= 5 && fields[3] == "table" {
				return state.showFIBTable(fields[4])
			}
			if len(fields) >= 4 {
				return state.showFIBPrefix(fields[3])
			}
		}
		if len(fields) >= 4 && fields[2] == "table" {
			return fields[3] + "\n"
		}
		if len(fields) >= 3 && fields[2] == "summary" {
			return "ipv4-VRF:0, fib_index:0\n"
		}
	case "fib":
		if len(fields) >= 3 && fields[2] == "path-lists" {
			return state.showPathLists()
		}
	case "acl-plugin":
		return state.showACLPlugin(fields)
	case "abf":
		return state.showABF(fields)
	case "nat44":
		return state.showNAT44(fields)
	case "qos":
		return state.showQoS(fields)
	case "policer":
		return state.showPolicers()
	case "tap":
		return joinLines(state.Taps)
	case "ly-route":
		return state.showLyRoute(fields)
	}
	return ""
}

func (state *State) showInterfaceAddress(only string) string {
	var builder strings.Builder
	for _, item := range state.Interfaces {
		if only != "" && item.Name != only {
			continue
		}
		fmt.Fprintf(&builder, "%s (%s):\n", item.Name, interfaceStateWord(item.Up))
		for _, address := range item.Addresses {
			fmt.Fprintf(&builder, "  L3 %s\n", address)
		}
	}
	return builder.String()
}

func interfaceStateWord(up bool) string {
	if up {
		return "up"
	}
	return "down"
}

func (state *State) showInterfaces() string {
	var builder strings.Builder
	builder.WriteString("Name Idx State MTU (L3/IP4/IP6/MPLS) Counter Count\n")
	for _, item := range state.Interfaces {
		fmt.Fprintf(&builder, "%s %d %s 9000/0/0/0\n", item.Name, item.Index, interfaceStateWord(item.Up))
	}
	for _, bond := range state.Bonds {
		fmt.Fprintf(&builder, "%s %d up 9000/0/0/0\n", bond.Name, bond.Index)
	}
	return builder.String()
}

func (state *State) showInterface(name string) string {
	if item := state.interfaceByName(name); item != nil {
		return fmt.Sprintf("%s %d %s\n", item.Name, item.Index, interfaceStateWord(item.Up))
	}
	for _, bond := range state.Bonds {
		if bond.Name == name {
			return fmt.Sprintf("%s %d up\n", bond.Name, bond.Index)
		}
	}
	return ""
}

func (state *State) showHardwareInterface(name string) string {
	item := state.interfaceByName(name)
	if item == nil {
		return ""
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "%s\n", item.Name)
	if item.Linux != "" {
		fmt.Fprintf(&builder, "  netdev %s\n", item.Linux)
	}
	builder.WriteString("  queue(s) rx:0 tx:0\n")
	return builder.String()
}

func (state *State) showLCP() string {
	var builder strings.Builder
	builder.WriteString("lcp default netns '<unset>'\n")
	for index, pair := range state.LCP {
		fmt.Fprintf(&builder, "itf-pair: [%d] %s tap%d %s %d type tap\n", index, pair.VPP, 4096+index, pair.Host, 36+index)
	}
	return builder.String()
}

func (state *State) showBonds() string {
	var builder strings.Builder
	for index, bond := range state.Bonds {
		fmt.Fprintf(&builder, "%s\n", bond.Name)
		fmt.Fprintf(&builder, "  mode: %s\n", bond.Mode)
		fmt.Fprintf(&builder, "  number of active members: %d\n", len(bond.Members))
		for _, member := range bond.Members {
			fmt.Fprintf(&builder, "    %s\n", member)
		}
		fmt.Fprintf(&builder, "  number of members: %d\n", len(bond.Members))
		for _, member := range bond.Members {
			fmt.Fprintf(&builder, "    %s\n", member)
		}
		fmt.Fprintf(&builder, "  device instance: %d\n", index)
		fmt.Fprintf(&builder, "  interface id: %d\n", bond.Index)
		fmt.Fprintf(&builder, "  sw_if_index: %d\n", bond.Index)
		fmt.Fprintf(&builder, "  hw_if_index: %d\n", bond.Index)
	}
	return builder.String()
}

func (state *State) showFIBTable(raw string) string {
	id, err := strconv.Atoi(raw)
	if err != nil {
		return ""
	}
	table := state.tableByID(id)
	if table == nil {
		return ""
	}
	return renderTable(table)
}

func renderTable(table *Table) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "ipv4-VRF:%d, fib_index:%d", table.ID, table.FibIndex)
	if table.FlowHash {
		builder.WriteString(", flow hash:[src dst sport dport proto]")
	}
	builder.WriteString("\n")
	for _, route := range table.Routes {
		fmt.Fprintf(&builder, "%s\n", route.Prefix)
		builder.WriteString("  unicast-ip4-chain\n")
		fmt.Fprintf(&builder, "    [@0]: dpo-load-balance: [proto:ip4 index:8 buckets:%d uRPF:7 to:[0:0]]\n", len(route.Paths))
		fmt.Fprintf(&builder, "      path-list:[17] locks:1 flags:shared len:%d\n", len(route.Paths))
		for index, path := range route.Paths {
			fmt.Fprintf(&builder, "        path:[%d] pl-index:17 ip4 weight=%d pref=%d\n", 21+index, path.Weight, path.Pref)
			fmt.Fprintf(&builder, "          [@%d]: ipv4 via %s\n", index, fibVia(path.Via))
		}
	}
	return builder.String()
}

func fibVia(via string) string {
	if strings.HasPrefix(via, "ip4-lookup-in-table ") {
		return "table " + strings.TrimSpace(strings.TrimPrefix(via, "ip4-lookup-in-table "))
	}
	return via
}

func (state *State) showFIBPrefix(prefix string) string {
	if prefix == "255.255.255.255" || prefix == "255.255.255.255/32" {
		if !state.Broadcast {
			return ""
		}
		return "255.255.255.255/32\n  [@12]: dpo-receive: 0.0.0.0 on local0\n"
	}
	return ""
}

func (state *State) showPathLists() string {
	var builder strings.Builder
	for _, table := range state.Tables {
		for _, route := range table.Routes {
			if len(route.Paths) == 0 {
				continue
			}
			fmt.Fprintf(&builder, "path-list:[18] locks:1 flags:shared len:%d\n", len(route.Paths))
			for index, path := range route.Paths {
				fmt.Fprintf(&builder, "  path:[%d] pl-index:18 ip4 weight=%d pref=%d\n", 22+index, path.Weight, path.Pref)
				if len(strings.Fields(path.Via)) >= 2 {
					fmt.Fprintf(&builder, "    %s\n", path.Via)
				} else {
					fmt.Fprintf(&builder, "    ipv4 via %s\n", path.Via)
				}
			}
		}
	}
	return builder.String()
}

func (state *State) showACLPlugin(fields []string) string {
	if len(fields) >= 3 && fields[2] == "interface" {
		return state.showACLInterface()
	}
	if len(fields) >= 3 && fields[2] == "acl" {
		if len(fields) >= 5 && fields[3] == "index" {
			id, err := strconv.Atoi(fields[4])
			if err != nil {
				return ""
			}
			acl := state.aclByID(id)
			if acl == nil {
				return ""
			}
			return renderACL(acl)
		}
		var builder strings.Builder
		for _, acl := range state.ACLs {
			builder.WriteString(renderACL(acl))
		}
		return builder.String()
	}
	if len(fields) >= 3 && fields[2] == "macip" {
		return ""
	}
	return ""
}

func renderACL(acl *ACL) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "acl-index %d count %d tag {%s}\n", acl.ID, len(acl.Rules), acl.Tag)
	for index, rule := range acl.Rules {
		family := "ipv4"
		if strings.Contains(rule, "::") {
			family = "ipv6"
		}
		fmt.Fprintf(&builder, "  %d: %s %s\n", index, family, rule)
	}
	return builder.String()
}

func (state *State) showACLInterface() string {
	var builder strings.Builder
	for _, item := range state.Interfaces {
		if len(item.InputACLs) == 0 && len(item.OutputACLs) == 0 {
			continue
		}
		fmt.Fprintf(&builder, "sw_if_index %d:\n", item.Index)
		if len(item.InputACLs) > 0 {
			fmt.Fprintf(&builder, "  input acl(s): %s\n", joinInts(item.InputACLs))
		}
		if len(item.OutputACLs) > 0 {
			fmt.Fprintf(&builder, "  output acl(s): %s\n", joinInts(item.OutputACLs))
		}
	}
	return builder.String()
}

func joinInts(values []int) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(value))
	}
	return strings.Join(parts, ", ")
}

func (state *State) showABF(fields []string) string {
	if len(fields) < 3 {
		return ""
	}
	if fields[2] == "attach" {
		if len(fields) >= 4 {
			return state.showABFAttach(fields[3])
		}
		var builder strings.Builder
		for _, item := range state.Interfaces {
			builder.WriteString(state.showABFAttach(item.Name))
		}
		return builder.String()
	}
	if fields[2] == "policy" && len(fields) >= 4 {
		id, err := strconv.Atoi(fields[3])
		if err != nil {
			return ""
		}
		policy := state.abfByID(id)
		if policy == nil {
			return fmt.Sprintf("Invalid policy ID:%d\n", id)
		}
		return state.renderABFPolicy(policy)
	}
	return ""
}

func (state *State) renderABFPolicy(policy *ABFPolicy) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "abf:[0]: policy:%d acl:%d\n", policy.ID, policy.ACLID)
	if policy.Local {
		builder.WriteString(" no forwarding\n")
		return builder.String()
	}
	builder.WriteString(" path-list:[17] locks:1 flags:shared len:1\n")
	builder.WriteString("  path:[21] pl-index:17 ip4 weight=1 pref=0\n")
	if strings.HasPrefix(policy.Via, "ip4-lookup-in-table ") {
		tableID := strings.TrimSpace(strings.TrimPrefix(policy.Via, "ip4-lookup-in-table "))
		fmt.Fprintf(&builder, "    [@0]: ipv4 via table %s\n", tableID)
		if table, err := strconv.Atoi(tableID); err == nil {
			if found := state.tableByID(table); found != nil {
				fmt.Fprintf(&builder, " fib-index:%d\n", found.FibIndex)
			}
		}
		return builder.String()
	}
	fmt.Fprintf(&builder, "    [@0]: ipv4 via %s\n", policy.Via)
	return builder.String()
}

func (state *State) showABFAttach(name string) string {
	var builder strings.Builder
	attached := []*ABFPolicy{}
	for _, policy := range state.ABFs {
		if policy.Ingress == name {
			attached = append(attached, policy)
		}
	}
	if len(attached) == 0 {
		return ""
	}
	fmt.Fprintf(&builder, "%s\n", name)
	builder.WriteString("ipv4:\n")
	for _, policy := range attached {
		fmt.Fprintf(&builder, " abf-interface-attach: policy:%d priority:%d\n", policy.ID, policy.Priority)
	}
	builder.WriteString("ipv6:\n")
	return builder.String()
}

func (state *State) showNAT44(fields []string) string {
	if len(fields) < 3 {
		return ""
	}
	ei := false
	index := 2
	if fields[index] == "ei" {
		ei = true
		index++
	}
	if ei {
		// NAT44-EI is not used by the endpoint-dependent gateway plan; render
		// empty inventories so a full-cone candidate fails closed if it ever
		// reaches this fake.
		return ""
	}
	if index >= len(fields) {
		return ""
	}
	switch fields[index] {
	case "static":
		output := state.renderNATMappings()
		return filterInclude(output, fields[index+1:])
	case "addresses":
		return joinLines(state.NATAddresses)
	case "interfaces":
		var builder strings.Builder
		for _, item := range state.Interfaces {
			if item.NATIn {
				fmt.Fprintf(&builder, "%s in\n", item.Name)
			}
		}
		return builder.String()
	case "sessions":
		return ""
	}
	return ""
}

func (state *State) renderNATMappings() string {
	var builder strings.Builder
	builder.WriteString("NAT44 static mappings:\n")
	for _, item := range state.NATStatic {
		fmt.Fprintf(&builder, "  local %s external %s vrf 0\n", item.Internal, item.External)
	}
	for _, item := range state.NATPorts {
		fmt.Fprintf(&builder, "  %s local %s:%d external %s:%d vrf 0\n", item.Protocol, item.InternalHost, item.InternalPort, item.External, item.ExternalPort)
	}
	return builder.String()
}

func filterInclude(output string, fields []string) string {
	if len(fields) == 0 || (len(fields) >= 1 && fields[0] != "|") {
		return output
	}
	needle := ""
	if len(fields) >= 3 && fields[1] == "include" {
		needle = fields[2]
	}
	if needle == "" {
		return output
	}
	var builder strings.Builder
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, needle) {
			builder.WriteString(line)
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func (state *State) showQoS(fields []string) string {
	if len(fields) < 3 {
		return ""
	}
	switch fields[2] {
	case "egress":
		if len(fields) < 6 || fields[3] != "map" || fields[4] != "id" {
			return ""
		}
		id, err := strconv.Atoi(fields[5])
		if err != nil || !state.qosMapExists(id) {
			return ""
		}
		values := state.qosMap(id)
		zeroes := make([]string, 256)
		for index := range zeroes {
			zeroes[index] = "0"
		}
		rows := make([]string, 256)
		copy(rows, zeroes)
		for index, value := range values {
			rows[index] = strconv.Itoa(value)
		}
		var builder strings.Builder
		fmt.Fprintf(&builder, "Map-ID:%d\n", id)
		fmt.Fprintf(&builder, "  ext:[%s]\n", strings.Join(zeroes, ","))
		fmt.Fprintf(&builder, "  VLAN:[%s]\n", strings.Join(zeroes, ","))
		fmt.Fprintf(&builder, "  MPLS:[%s]\n", strings.Join(zeroes, ","))
		fmt.Fprintf(&builder, "  IP:[%s]\n", strings.Join(rows, ","))
		return builder.String()
	case "mark":
		if len(fields) < 4 {
			return ""
		}
		item := state.interfaceByName(fields[3])
		if item == nil || item.Mark == 0 {
			return ""
		}
		return fmt.Sprintf("%s:\n  IP: map:%d\n", item.Name, item.Mark)
	case "record":
		if len(fields) < 4 {
			return ""
		}
		item := state.interfaceByName(fields[3])
		if item == nil || !item.Record {
			return ""
		}
		return fmt.Sprintf("%s:\n  IP\n", item.Name)
	case "store":
		if len(fields) < 4 {
			return ""
		}
		item := state.interfaceByName(fields[3])
		if item == nil || !item.HasStore {
			return ""
		}
		return fmt.Sprintf("%s:\n  IP -> %d\n", item.Name, item.Store)
	}
	return ""
}

func (state *State) showPolicers() string {
	var builder strings.Builder
	for _, name := range state.Policers {
		fmt.Fprintf(&builder, "Name \"%s\" type 1r2c cir 0 eir 0 cb 0 eb 0\n", name)
		builder.WriteString("-----------\n")
	}
	return builder.String()
}

func (state *State) showLyRoute(fields []string) string {
	if len(fields) < 3 {
		return ""
	}
	switch fields[2] {
	case "pre-nat-route":
		return state.renderPreNAT()
	case "dns-intercept":
		if !state.DNS.Enabled {
			return "enabled 0\n"
		}
		return fmt.Sprintf("enabled 1 interface %s table %d\n", state.DNS.Interface, state.DNS.Table)
	case "security-guard":
		return joinLines(state.GuardRules)
	case "flow-rate":
		return joinLines(state.FlowRates)
	case "smart-qos":
		return ""
	}
	return ""
}

func (state *State) renderPreNAT() string {
	enabled := 0
	if state.PreNAT.Enabled {
		enabled = 1
	}
	interfaceName := state.PreNAT.Interface
	prefix := state.PreNAT.LANPrefix
	if prefix == "" {
		prefix = "0.0.0.0/0"
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "enabled %d interface %s lan-prefix %s rules %d radix-nodes 0\n", enabled, interfaceName, prefix, len(state.PreNAT.Rules))
	for _, rule := range state.PreNAT.Rules {
		fmt.Fprintf(&builder, "rule id %d priority %d prefixes %d table %d fib-index %d skip-nat %d bypass %d\n",
			rule.ID, rule.Priority, rule.Prefixes, rule.Table, rule.FibIndex, boolInt(rule.SkipNAT), boolInt(rule.Bypass))
	}
	return builder.String()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}
