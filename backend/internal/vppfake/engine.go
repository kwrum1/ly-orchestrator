package vppfake

import (
	"fmt"
	"strconv"
	"strings"
)

// Execute applies one VPP CLI line to the emulated state and returns the same
// triple a real vppctl invocation would produce: stdout, stderr, exit code.
func Execute(state *State, command string) (string, string, int) {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return "", "", 0
	}
	switch fields[0] {
	case "show":
		return state.show(fields), "", 0
	case "create":
		state.create(fields)
	case "delete":
		state.deleteObject(fields)
	case "set":
		if len(fields) >= 3 && fields[1] == "acl-plugin" && fields[2] == "acl" {
			return state.createACL(fields), "", 0
		}
		state.set(fields)
	case "ip":
		state.ip(fields)
	case "bond":
		state.bond(fields)
	case "lcp":
		state.lcp(fields)
	case "abf":
		state.abf(fields)
	case "nat44":
		state.nat44(fields)
	case "qos":
		state.qos(fields)
	case "policer":
		state.policer(fields)
	case "clear":
		return "", "", 0
	default:
		// Unknown mutations are accepted silently, like an idempotent CLI
		// plugin command with no readback surface.
	}
	return "", "", 0
}

func (state *State) create(fields []string) {
	if len(fields) >= 2 && fields[1] == "bond" {
		state.createBond(fields)
		return
	}
	if len(fields) < 3 || fields[1] != "interface" {
		return
	}
	kind := fields[2]
	linux := ""
	name := ""
	for index := 3; index+1 < len(fields); index++ {
		switch fields[index] {
		case "host-if":
			linux = fields[index+1]
		case "name", "host-if-name":
			name = fields[index+1]
		}
	}
	if name == "" {
		name = linux
	}
	if name == "" {
		return
	}
	item := state.ensureInterface(name)
	item.Kind = kind
	item.Hardware = true
	if linux != "" {
		item.Linux = linux
	}
	if kind == "tap" {
		state.Taps = appendUniqueString(state.Taps, name)
	}
}

func (state *State) createBond(fields []string) {
	// create bond mode <mode> id <n>
	mode := ""
	id := 0
	for index := 2; index+1 < len(fields); index++ {
		switch fields[index] {
		case "mode":
			mode = fields[index+1]
		case "id":
			id, _ = strconv.Atoi(fields[index+1])
		}
	}
	if id == 0 {
		return
	}
	name := fmt.Sprintf("BondEthernet%d", id)
	for _, bond := range state.Bonds {
		if bond.Name == name || bond.Index == id {
			bond.Mode = mode
			return
		}
	}
	state.Bonds = append(state.Bonds, &Bond{Name: name, Mode: mode, Index: id})
}

func (state *State) deleteObject(fields []string) {
	if len(fields) < 3 {
		return
	}
	switch fields[1] {
	case "interface":
		name := fields[len(fields)-1]
		state.removeInterface(name)
	case "bond":
		state.removeBond(fields[2])
	case "tap":
		state.Taps = removeString(state.Taps, fields[len(fields)-1])
	case "acl-plugin":
		if len(fields) >= 5 && fields[2] == "acl" && fields[3] == "index" {
			if id, err := strconv.Atoi(fields[4]); err == nil {
				state.removeACL(id)
			}
		}
	}
}

func (state *State) removeInterface(name string) {
	kept := state.Interfaces[:0]
	for _, item := range state.Interfaces {
		if item.Name != name {
			kept = append(kept, item)
		}
	}
	state.Interfaces = kept
	for _, item := range state.ABFs {
		if item.Ingress == name {
			item.Ingress = ""
			item.Priority = 0
		}
	}
	keptPairs := state.LCP[:0]
	for _, pair := range state.LCP {
		if pair.VPP != name {
			keptPairs = append(keptPairs, pair)
		}
	}
	state.LCP = keptPairs
}

func (state *State) removeBond(name string) {
	kept := state.Bonds[:0]
	for _, item := range state.Bonds {
		if item.Name != name {
			kept = append(kept, item)
		}
	}
	state.Bonds = kept
}

func (state *State) bond(fields []string) {
	if len(fields) < 3 {
		return
	}
	switch fields[1] {
	case "add":
		for _, item := range state.Bonds {
			if item.Name == fields[2] && len(fields) > 3 {
				item.Members = appendUniqueString(item.Members, fields[3])
			}
		}
	case "del":
		for _, item := range state.Bonds {
			if item.Name == fields[2] && len(fields) > 3 {
				item.Members = removeString(item.Members, fields[3])
			}
		}
	}
}

func (state *State) lcp(fields []string) {
	if len(fields) < 2 {
		return
	}
	switch fields[1] {
	case "create":
		if len(fields) >= 5 {
			state.LCP = append(state.LCP, LCPPair{VPP: fields[2], Host: fields[4]})
		}
	case "delete":
		if len(fields) >= 3 {
			kept := state.LCP[:0]
			for _, pair := range state.LCP {
				if pair.VPP != fields[2] {
					kept = append(kept, pair)
				}
			}
			state.LCP = kept
		}
	}
}

func (state *State) set(fields []string) {
	if len(fields) < 2 {
		return
	}
	switch fields[1] {
	case "interface":
		state.setInterface(fields)
	case "acl-plugin":
		state.setACLPlugin(fields)
	case "ip":
		state.setIP(fields)
	case "ly-route":
		state.setLyRoute(fields)
	}
}

func (state *State) setInterface(fields []string) {
	if len(fields) < 3 {
		return
	}
	switch fields[2] {
	case "state":
		if len(fields) >= 5 {
			if item := state.interfaceByName(fields[3]); item != nil {
				item.Up = fields[4] == "up"
			}
		}
	case "name":
		if len(fields) >= 5 {
			oldName, newName := fields[3], fields[4]
			if item := state.interfaceByName(oldName); item != nil {
				item.Name = newName
			}
			for _, bond := range state.Bonds {
				if bond.Name == oldName {
					bond.Name = newName
				}
			}
		}
	case "mtu":
		// set interface mtu packet <n> <name>
		if len(fields) >= 6 {
			_ = fields[4]
		}
	case "ip":
		state.setInterfaceIP(fields)
	case "input", "output":
		state.setInterfaceACL(fields)
	case "nat44":
		state.setInterfaceNAT(fields)
	}
}

func (state *State) setInterfaceIP(fields []string) {
	// set interface ip address [del] <name> <cidr>
	if len(fields) < 5 || fields[3] != "address" {
		if len(fields) >= 5 && fields[3] == "table" {
			return
		}
		return
	}
	deleting := false
	index := 4
	if fields[index] == "del" {
		deleting = true
		index++
	}
	if len(fields) < index+2 {
		return
	}
	name, cidr := fields[index], fields[index+1]
	item := state.ensureInterface(name)
	if deleting {
		item.Addresses = removeString(item.Addresses, cidr)
		return
	}
	item.Addresses = appendUniqueString(item.Addresses, cidr)
}

func (state *State) setInterfaceACL(fields []string) {
	// set interface input|output acl intfc <iface> ip4-table <id> [del]
	if len(fields) < 8 || fields[3] != "acl" || fields[4] != "intfc" {
		return
	}
	direction := fields[2]
	name := fields[5]
	if fields[6] != "ip4-table" {
		return
	}
	id, err := strconv.Atoi(fields[7])
	if err != nil {
		return
	}
	item := state.ensureInterface(name)
	deleting := len(fields) > 8 && fields[8] == "del"
	switch direction {
	case "input":
		item.InputACLs = applyACLAttachment(item.InputACLs, id, deleting)
	case "output":
		item.OutputACLs = applyACLAttachment(item.OutputACLs, id, deleting)
	}
}

func (state *State) setInterfaceNAT(fields []string) {
	// set interface nat44 [ei] in|out <iface> [output-feature] [del]
	index := 3
	ei := false
	if index < len(fields) && fields[index] == "ei" {
		ei = true
		index++
	}
	if index >= len(fields) {
		return
	}
	direction := fields[index]
	index++
	if index >= len(fields) {
		return
	}
	name := fields[index]
	item := state.ensureInterface(name)
	deleting := false
	for _, field := range fields[index+1:] {
		if field == "del" {
			deleting = true
		}
	}
	if ei {
		item.NATEIIn = direction == "in" && !deleting
		return
	}
	if direction == "in" {
		item.NATIn = !deleting
	}
}

func (state *State) setACLPlugin(fields []string) {
	if len(fields) < 4 {
		return
	}
	switch fields[2] {
	case "interface":
		state.attachACL(fields)
	case "macip":
		// MACIP state is not modelled; absence is rendered as empty output.
	}
}

func (state *State) createACL(fields []string) string {
	// set acl-plugin acl [index N] <rules...> tag <tag>
	index := 3
	explicit := 0
	if len(fields) > 4 && fields[3] == "index" {
		id, err := strconv.Atoi(fields[4])
		if err != nil {
			return ""
		}
		explicit = id
		index = 5
	}
	rest := strings.Join(fields[index:], " ")
	tag := ""
	if position := strings.Index(rest, " tag "); position >= 0 {
		// VPP accepts the ACL tag before or after the rule list:
		//   set acl-plugin acl <rules> tag <tag>
		//   set acl-plugin acl <rules>, tag <tag> <rules>
		after := strings.TrimSpace(rest[position+5:])
		tag = after
		trailing := ""
		if space := strings.IndexAny(after, " \t"); space >= 0 {
			tag = after[:space]
			trailing = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(after[space:]), ","))
		}
		before := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(rest[:position]), ","))
		rest = before
		if trailing != "" {
			if rest != "" {
				rest += ", "
			}
			rest += trailing
		}
	}
	rules := []string{}
	for _, rule := range strings.Split(rest, ",") {
		rule = strings.TrimSpace(rule)
		if rule != "" {
			rules = append(rules, rule)
		}
	}
	if len(rules) == 0 {
		return ""
	}
	id := explicit
	if id == 0 {
		id = state.nextACLID()
	}
	if existing := state.aclByID(id); existing != nil {
		existing.Tag = tag
		existing.Rules = rules
	} else {
		state.ACLs = append(state.ACLs, &ACL{ID: id, Tag: tag, Rules: rules})
	}
	return fmt.Sprintf("index:%d\n", id)
}

func (state *State) attachACL(fields []string) {
	// set acl-plugin interface <iface> <direction> acl <id> [del]
	if len(fields) < 7 || fields[5] != "acl" {
		return
	}
	name, direction := fields[3], fields[4]
	id, err := strconv.Atoi(fields[6])
	if err != nil {
		return
	}
	deleting := len(fields) > 7 && fields[7] == "del"
	item := state.ensureInterface(name)
	switch direction {
	case "input":
		item.InputACLs = applyACLAttachment(item.InputACLs, id, deleting)
	case "output":
		item.OutputACLs = applyACLAttachment(item.OutputACLs, id, deleting)
	}
}

func applyACLAttachment(values []int, id int, deleting bool) []int {
	if deleting {
		return removeInt(values, id)
	}
	for _, value := range values {
		if value == id {
			return values
		}
	}
	return append([]int{id}, values...)
}

func (state *State) setIP(fields []string) {
	// set ip flow-hash table <id> ...
	if len(fields) >= 5 && fields[2] == "flow-hash" && fields[3] == "table" {
		if id, err := strconv.Atoi(fields[4]); err == nil {
			state.ensureTable(id).FlowHash = true
		}
	}
}

func (state *State) setLyRoute(fields []string) {
	if len(fields) < 4 {
		return
	}
	switch fields[2] {
	case "dns-intercept":
		state.setDNSIntercept(fields)
	case "pre-nat-route":
		state.setPreNAT(fields)
	case "security-guard":
		state.setSecurityGuard(fields)
	case "flow-rate":
		state.setFlowRate(fields)
	}
}

func (state *State) setDNSIntercept(fields []string) {
	// set ly-route dns-intercept interface <iface> table <table>
	if fields[3] == "disable" {
		state.DNS = DNSIntercept{}
		return
	}
	if len(fields) >= 6 && fields[3] == "interface" {
		state.DNS.Enabled = true
		state.DNS.Interface = fields[4]
		if len(fields) >= 8 && fields[6] == "table" {
			if table, err := strconv.Atoi(fields[7]); err == nil {
				state.DNS.Table = table
			}
		}
	}
}

func (state *State) setPreNAT(fields []string) {
	if len(fields) < 4 {
		return
	}
	switch fields[3] {
	case "interface":
		// set ly-route pre-nat-route interface <iface> lan-prefix <prefix>
		state.PreNAT.Enabled = true
		if len(fields) >= 8 {
			state.PreNAT.Interface = fields[4]
			state.PreNAT.LANPrefix = fields[6]
		}
	case "add":
		state.PreNAT.Enabled = true
		rule := PreNATRule{}
		for index := 4; index < len(fields); index++ {
			key := fields[index]
			if key == "bypass" || key == "skip-nat" {
				enabled := true
				if index+1 < len(fields) && (fields[index+1] == "0" || fields[index+1] == "1") {
					enabled = fields[index+1] == "1"
					index++
				}
				if key == "bypass" {
					rule.Bypass = enabled
				} else {
					rule.SkipNAT = enabled
				}
				continue
			}
			if index+1 >= len(fields) {
				break
			}
			value := fields[index+1]
			index++
			switch key {
			case "id":
				rule.ID, _ = strconv.Atoi(value)
			case "priority":
				rule.Priority, _ = strconv.Atoi(value)
			case "source":
				rule.Source = value
			case "destination":
				rule.Dest = value
			case "protocol":
				rule.Protocol = value
			case "sport":
				rule.Sport = value
			case "dport":
				rule.Dport = value
			case "table":
				rule.Table, _ = strconv.Atoi(value)
				if table := state.tableByID(rule.Table); table != nil {
					rule.FibIndex = table.FibIndex
				}
			}
		}
		state.PreNAT.Rules = append(state.PreNAT.Rules, rule)
	case "del":
		if len(fields) >= 6 && fields[4] == "id" {
			id, err := strconv.Atoi(fields[5])
			if err != nil {
				return
			}
			kept := state.PreNAT.Rules[:0]
			for _, rule := range state.PreNAT.Rules {
				if rule.ID != id {
					kept = append(kept, rule)
				}
			}
			state.PreNAT.Rules = kept
			state.removeABF(id)
			state.removeNATGuard(id)
		}
	}
}

func (state *State) setSecurityGuard(fields []string) {
	// set ly-route security-guard rule <id> enabled <0|1> family ...
	line := strings.Join(fields[3:], " ")
	if !strings.HasPrefix(line, "rule ") {
		return
	}
	state.GuardRules = append(state.GuardRules, line)
}

func (state *State) setFlowRate(fields []string) {
	line := strings.Join(fields[3:], " ")
	if !strings.HasPrefix(line, "rule ") {
		return
	}
	state.FlowRates = append(state.FlowRates, line)
}

func (state *State) ip(fields []string) {
	if len(fields) < 3 {
		return
	}
	switch fields[1] {
	case "table":
		if len(fields) < 4 {
			return
		}
		id, err := strconv.Atoi(fields[3])
		if err != nil {
			return
		}
		switch fields[2] {
		case "add":
			state.ensureTable(id)
		case "del":
			state.removeTable(id)
		}
	case "route":
		state.ipRoute(fields)
	}
}

func (state *State) ipRoute(fields []string) {
	// ip route add|del [table <id>] <prefix> via <paths...>
	if len(fields) < 5 {
		return
	}
	action := fields[2]
	index := 3
	tableID := 0
	if fields[index] == "table" && len(fields) > index+2 {
		tableID, _ = strconv.Atoi(fields[index+1])
		index += 2
	}
	if index >= len(fields) {
		return
	}
	prefix := fields[index]
	paths := []Path{}
	if index+2 < len(fields) && fields[index+1] == "via" {
		paths = parsePaths(strings.Join(fields[index+2:], " "))
	}
	if action == "del" {
		if tableID == 0 {
			if prefix == "255.255.255.255/32" {
				state.Broadcast = false
			}
			return
		}
		if table := state.tableByID(tableID); table != nil {
			table.Routes = removeRoute(table.Routes, prefix, paths)
		}
		return
	}
	if tableID == 0 {
		if prefix == "255.255.255.255/32" {
			for _, path := range paths {
				if path.Via == "local" {
					state.Broadcast = true
				}
			}
		}
		return
	}
	table := state.ensureTable(tableID)
	table.Routes = replaceRoute(table.Routes, Route{Prefix: canonicalPrefix(prefix), Paths: paths})
}

func parsePaths(text string) []Path {
	chunks := strings.Split(text, " via ")
	paths := make([]Path, 0, len(chunks))
	for _, chunk := range chunks {
		fields := strings.Fields(chunk)
		path := Path{Weight: 1}
		via := []string{}
		for index := 0; index < len(fields); index++ {
			switch fields[index] {
			case "weight":
				if index+1 < len(fields) {
					path.Weight, _ = strconv.Atoi(fields[index+1])
					index++
				}
			case "preference":
				if index+1 < len(fields) {
					path.Pref, _ = strconv.Atoi(fields[index+1])
					index++
				}
			default:
				via = append(via, fields[index])
			}
		}
		if len(via) == 0 {
			continue
		}
		path.Via = strings.Join(via, " ")
		if path.Weight < 1 {
			path.Weight = 1
		}
		paths = append(paths, path)
	}
	return paths
}

func canonicalPrefix(value string) string {
	value = strings.TrimSpace(value)
	address, suffix, found := strings.Cut(value, "/")
	if !found {
		return value
	}
	bits, err := strconv.Atoi(suffix)
	if err != nil {
		return value
	}
	octets := strings.Split(address, ".")
	if len(octets) != 4 {
		return value
	}
	numbers := make([]int, 4)
	for index, octet := range octets {
		numbers[index], _ = strconv.Atoi(octet)
	}
	mask := uint32(0xffffffff)
	if bits < 32 {
		mask = ^(uint32(1<<(32-bits)) - 1)
	}
	if bits == 0 {
		mask = 0
	}
	value32 := uint32(numbers[0])<<24 | uint32(numbers[1])<<16 | uint32(numbers[2])<<8 | uint32(numbers[3])
	value32 &= mask
	return fmt.Sprintf("%d.%d.%d.%d/%d", byte(value32>>24), byte(value32>>16), byte(value32>>8), byte(value32), bits)
}

func replaceRoute(routes []Route, replacement Route) []Route {
	updated := make([]Route, 0, len(routes)+1)
	for _, route := range routes {
		if route.Prefix != replacement.Prefix {
			updated = append(updated, route)
		}
	}
	return append(updated, replacement)
}

func removeRoute(routes []Route, prefix string, paths []Path) []Route {
	kept := make([]Route, 0, len(routes))
	for _, route := range routes {
		if route.Prefix != prefix {
			kept = append(kept, route)
			continue
		}
		if len(paths) == 0 {
			continue
		}
		remaining := []Path{}
		for _, existing := range route.Paths {
			drop := false
			for _, unwanted := range paths {
				if existing.Via == unwanted.Via {
					drop = true
				}
			}
			if !drop {
				remaining = append(remaining, existing)
			}
		}
		if len(remaining) > 0 {
			route.Paths = remaining
			kept = append(kept, route)
		}
	}
	return kept
}

func (state *State) abf(fields []string) {
	if len(fields) < 3 {
		return
	}
	switch fields[1] {
	case "policy":
		state.abfPolicy(fields)
	case "attach":
		state.abfAttach(fields)
	}
}

func (state *State) abfPolicy(fields []string) {
	// abf policy add id <id> acl <acl> via <via...>
	// abf policy del id <id> [acl <acl> via <via...>]
	if len(fields) < 4 {
		return
	}
	action := fields[2]
	values := map[string]string{}
	rest := []string{}
	for index := 3; index < len(fields); index++ {
		switch fields[index] {
		case "id", "acl":
			if index+1 < len(fields) {
				values[fields[index]] = fields[index+1]
				index++
			}
		case "via":
			rest = fields[index+1:]
			index = len(fields)
		}
	}
	id, err := strconv.Atoi(values["id"])
	if err != nil {
		return
	}
	if action == "del" {
		state.removeABF(id)
		return
	}
	aclID, err := strconv.Atoi(values["acl"])
	if err != nil {
		return
	}
	via := strings.TrimSpace(strings.Join(rest, " "))
	policy := state.abfByID(id)
	if policy == nil {
		policy = &ABFPolicy{ID: id}
		state.ABFs = append(state.ABFs, policy)
	}
	policy.ACLID = aclID
	policy.Via = via
	policy.Family = "ip4"
	policy.Local = via == "local"
}

func (state *State) abfAttach(fields []string) {
	// abf attach <family> policy <id> priority <p> <iface>
	// abf attach <family> del policy <id> <iface>
	if len(fields) < 5 {
		return
	}
	family := fields[2]
	deleting := fields[3] == "del"
	index := 3
	if deleting {
		index = 4
	}
	if index >= len(fields) || fields[index] != "policy" || index+1 >= len(fields) {
		return
	}
	id, err := strconv.Atoi(fields[index+1])
	if err != nil {
		return
	}
	policy := state.abfByID(id)
	if policy == nil {
		return
	}
	if deleting {
		for _, field := range fields[index+2:] {
			if field == policy.Ingress {
				policy.Ingress = ""
				policy.Priority = 0
			}
		}
		return
	}
	priority := 0
	ingress := ""
	for cursor := index + 2; cursor+1 < len(fields); cursor += 2 {
		if fields[cursor] == "priority" {
			priority, _ = strconv.Atoi(fields[cursor+1])
		} else if fields[cursor] == "interface" {
			ingress = fields[cursor+1]
		}
	}
	if ingress == "" {
		ingress = fields[len(fields)-1]
	}
	policy.Family = family
	policy.Ingress = ingress
	policy.Priority = priority
}

func (state *State) nat44(fields []string) {
	if len(fields) < 3 {
		return
	}
	index := 1
	if fields[index] == "ei" {
		index++
	}
	if index >= len(fields) {
		return
	}
	switch fields[index] {
	case "add":
		state.natAdd(fields[index+1:])
	}
}

func (state *State) natAdd(fields []string) {
	if len(fields) == 0 {
		return
	}
	switch fields[0] {
	case "address":
		if len(fields) >= 2 {
			state.NATAddresses = appendUniqueString(state.NATAddresses, fields[1])
		}
	case "static":
		state.natMapping(fields)
	}
}

func (state *State) natMapping(fields []string) {
	// add static mapping [proto] local <host> [port] external <addr> [port] [del]
	if len(fields) < 4 || fields[0] != "static" || fields[1] != "mapping" {
		return
	}
	rest := append([]string{}, fields[2:]...)
	deleting := false
	if len(rest) > 0 && rest[len(rest)-1] == "del" {
		deleting = true
		rest = rest[:len(rest)-1]
	}
	protocol := ""
	if len(rest) > 0 && (rest[0] == "tcp" || rest[0] == "udp") {
		protocol = rest[0]
		rest = rest[1:]
	}
	values := map[string]string{}
	for index := 0; index < len(rest); index++ {
		switch rest[index] {
		case "local":
			if index+1 < len(rest) {
				values["local"] = rest[index+1]
				index++
				if index+1 < len(rest) && isNumeric(rest[index+1]) {
					values["localPort"] = rest[index+1]
					index++
				}
			}
		case "external":
			if index+1 < len(rest) {
				values["external"] = rest[index+1]
				index++
				if index+1 < len(rest) && isNumeric(rest[index+1]) {
					values["externalPort"] = rest[index+1]
					index++
				}
			}
		}
	}
	local, external := values["local"], values["external"]
	if local == "" || external == "" {
		return
	}
	internalHost, internalPort := local, 0
	if strings.Contains(local, ":") {
		host, port, found := strings.Cut(local, ":")
		if found {
			internalHost = host
			internalPort, _ = strconv.Atoi(port)
		}
	}
	if values["localPort"] != "" {
		internalPort, _ = strconv.Atoi(values["localPort"])
	}
	externalAddress, externalPort := external, 0
	if strings.Contains(external, ":") {
		host, port, found := strings.Cut(external, ":")
		if found {
			externalAddress = host
			externalPort, _ = strconv.Atoi(port)
		}
	}
	if values["externalPort"] != "" {
		externalPort, _ = strconv.Atoi(values["externalPort"])
	}
	if protocol == "" {
		id := state.natStaticID(internalHost, externalAddress)
		if deleting {
			state.removeNATStatic(internalHost, externalAddress)
			return
		}
		state.NATStatic = append(state.NATStatic, NATStatic{ID: id, Internal: internalHost, External: externalAddress})
		state.installNATGuard(id, externalAddress, internalHost, 0, "any")
		return
	}
	id := state.natPortID(protocol, internalHost, internalPort, externalAddress, externalPort)
	if deleting {
		state.removeNATPort(id)
		state.removeNATGuardByID(id)
		return
	}
	state.NATPorts = append(state.NATPorts, NATPort{ID: id, Protocol: protocol, InternalHost: internalHost, InternalPort: internalPort, External: externalAddress, ExternalPort: externalPort})
	state.installNATGuard(id, externalAddress, internalHost, internalPort, protocol)
}

func (state *State) natStaticID(internal, external string) string {
	for _, item := range state.NATStatic {
		if item.Internal == internal && item.External == external {
			return item.ID
		}
	}
	return "static-" + safeTag(internal+"-"+external)
}

func (state *State) natPortID(protocol, host string, internalPort int, external string, externalPort int) string {
	for _, item := range state.NATPorts {
		if item.Protocol == protocol && item.InternalHost == host && item.InternalPort == internalPort && item.External == external && item.ExternalPort == externalPort {
			return item.ID
		}
	}
	return "port-" + safeTag(fmt.Sprintf("%s-%s-%d-%s-%d", protocol, host, internalPort, external, externalPort))
}

func (state *State) removeNATStatic(internal, external string) {
	kept := state.NATStatic[:0]
	for _, item := range state.NATStatic {
		if item.Internal != internal || item.External != external {
			kept = append(kept, item)
		} else {
			state.removeNATGuardByID(item.ID)
		}
	}
	state.NATStatic = kept
}

func (state *State) removeNATPort(id string) {
	kept := state.NATPorts[:0]
	for _, item := range state.NATPorts {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	state.NATPorts = kept
}

// installNATGuard mirrors the product's NAT44 lifecycle: every mapping owns a
// permit ACL for the external source, an ABF policy attached to the LAN, and a
// pre-NAT bypass rule so return traffic is not re-classified by policy order.
func (state *State) installNATGuard(id, external, internal string, internalPort int, protocol string) {
	policyID, tag := NATGuardFor(id)
	if existing := state.abfByID(policyID); existing != nil {
		return
	}
	prefix := external
	if !strings.Contains(prefix, "/") {
		prefix += "/32"
	}
	aclID := state.nextACLID()
	state.ACLs = append(state.ACLs, &ACL{ID: aclID, Tag: tag, Rules: []string{
		fmt.Sprintf("permit src %s dst 0.0.0.0/0 proto 0 sport 0-65535 dport 0-65535", prefix),
	}})
	via := "ip4-lookup-in-table 0"
	state.ABFs = append(state.ABFs, &ABFPolicy{ID: policyID, ACLID: aclID, Via: via, Family: "ip4", Ingress: state.PreNAT.Interface, Priority: 2})
	if state.PreNAT.Interface == "" && len(state.Interfaces) > 0 {
		state.ABFs[len(state.ABFs)-1].Ingress = state.Interfaces[0].Name
	}
	sport := "0-65535"
	if internalPort != 0 {
		sport = fmt.Sprintf("%d-%d", internalPort, internalPort)
	}
	state.PreNAT.Enabled = true
	state.PreNAT.Rules = append(state.PreNAT.Rules, PreNATRule{
		ID: policyID, Priority: 0, Table: 0, FibIndex: 0, Bypass: true,
		Source: internal, Dest: "0.0.0.0/0", Protocol: protocol, Sport: sport, Dport: "0-65535",
	})
}

func (state *State) removeNATGuardByID(id string) {
	policyID, tag := NATGuardFor(id)
	state.removeABF(policyID)
	keptRules := state.PreNAT.Rules[:0]
	for _, rule := range state.PreNAT.Rules {
		if rule.ID != policyID {
			keptRules = append(keptRules, rule)
		}
	}
	state.PreNAT.Rules = keptRules
	keptACLs := state.ACLs[:0]
	for _, acl := range state.ACLs {
		if acl.Tag != tag {
			keptACLs = append(keptACLs, acl)
		}
	}
	state.ACLs = keptACLs
}

func (state *State) removeNATGuard(policyID int) {
	for _, item := range state.NATStatic {
		if id, _ := NATGuardFor(item.ID); id == policyID {
			state.removeNATGuardByID(item.ID)
			return
		}
	}
	for _, item := range state.NATPorts {
		if id, _ := NATGuardFor(item.ID); id == policyID {
			state.removeNATGuardByID(item.ID)
			return
		}
	}
}

func (state *State) qos(fields []string) {
	if len(fields) < 2 {
		return
	}
	switch fields[1] {
	case "egress":
		// qos egress map id <id> [<source>][<class>]=<value>
		if len(fields) < 6 || fields[2] != "map" || fields[3] != "id" {
			return
		}
		id, err := strconv.Atoi(fields[4])
		if err != nil {
			return
		}
		values := state.qosMap(id)
		for _, token := range fields[5:] {
			source, rest, found := strings.Cut(token, "][")
			if !found {
				continue
			}
			source = strings.TrimPrefix(source, "[")
			rest = strings.TrimSuffix(rest, "]")
			indexText, valueText, found := strings.Cut(rest, "]=")
			if !found {
				continue
			}
			index, indexErr := strconv.Atoi(indexText)
			value, valueErr := strconv.Atoi(valueText)
			if indexErr != nil || valueErr != nil || index < 0 || index > 255 {
				continue
			}
			if strings.EqualFold(source, "ip") {
				values[index] = value
			}
		}
		state.QoSMaps[fmt.Sprintf("%d", id)] = values
	case "mark":
		// qos mark ip <iface> id <id>
		if len(fields) >= 6 {
			if item := state.interfaceByName(fields[3]); item != nil {
				id, _ := strconv.Atoi(fields[5])
				item.Mark = id
			}
		}
	case "record":
		if len(fields) >= 4 {
			if item := state.interfaceByName(fields[3]); item != nil {
				item.Record = true
			}
		}
	case "store":
		if len(fields) >= 6 {
			if item := state.interfaceByName(fields[3]); item != nil {
				value, _ := strconv.Atoi(fields[5])
				item.Store = value
				item.HasStore = true
			}
		}
	case "delete":
		if len(fields) >= 3 {
			delete(state.QoSMaps, fields[2])
		}
	}
}

func (state *State) policer(fields []string) {
	if len(fields) < 3 {
		return
	}
	switch fields[1] {
	case "add":
		state.Policers = appendUniqueString(state.Policers, fields[3])
	case "del":
		state.Policers = removeString(state.Policers, fields[3])
	}
}

func appendUniqueString(values []string, addition string) []string {
	for _, value := range values {
		if value == addition {
			return values
		}
	}
	return append(values, addition)
}

func isNumeric(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func removeString(values []string, unwanted string) []string {
	kept := values[:0]
	for _, value := range values {
		if value != unwanted {
			kept = append(kept, value)
		}
	}
	return kept
}
