// Package vppfake implements a deterministic, stateful stand-in for the VPP
// command line used by the production gateway proof harness.
//
// The harness previously answered `show` commands from a static command →
// output table. That made it impossible to prove ordering contracts: a
// `show abf policy 65000` issued before the policy existed returned the same
// bytes as a readback issued after it was created, and dynamically allocated
// objects (ACLs, ABF policies, attachments, NAT return guards, private FIB
// tables) could not be observed at all.
//
// This package models the small, well-defined slice of the VPP data plane the
// gateway transaction owns. Mutation commands change state; `show` commands
// render state. An object that was never created is therefore absent, exactly
// like on a live node, and a readback only succeeds after the corresponding
// mutation actually ran.
package vppfake

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Interface is an emulated VPP software interface (AF_XDP, TAP or bond).
type Interface struct {
	Name       string   `json:"name"`
	Index      int      `json:"index"`
	Up         bool     `json:"up"`
	Addresses  []string `json:"addresses,omitempty"`
	Linux      string   `json:"linux,omitempty"`
	Hardware   bool     `json:"hardware,omitempty"`
	InputACLs  []int    `json:"input_acls,omitempty"`
	OutputACLs []int    `json:"output_acls,omitempty"`
	Record     bool     `json:"record,omitempty"`
	Store      int      `json:"store,omitempty"`
	HasStore   bool     `json:"has_store,omitempty"`
	Mark       int      `json:"mark,omitempty"`
	NATIn      bool     `json:"nat_in,omitempty"`
	NATEIIn    bool     `json:"nat_ei_in,omitempty"`
	Kind       string   `json:"kind,omitempty"`
}

// Bond is an emulated bond interface.
type Bond struct {
	Name    string   `json:"name"`
	Mode    string   `json:"mode"`
	Members []string `json:"members"`
	Index   int      `json:"index"`
}

// Route is one prefix inside a private FIB table.
type Route struct {
	Prefix string `json:"prefix"`
	Paths  []Path `json:"paths"`
}

// Path is a single forwarding path of a FIB prefix.
type Path struct {
	Via    string `json:"via"`
	Weight int    `json:"weight"`
	Pref   int    `json:"pref"`
}

// Table is a private IPv4 FIB table.
type Table struct {
	ID       int     `json:"id"`
	FibIndex int     `json:"fib_index"`
	FlowHash bool    `json:"flow_hash,omitempty"`
	Routes   []Route `json:"routes,omitempty"`
}

// ACL is an emulated acl-plugin ACL.
type ACL struct {
	ID    int      `json:"id"`
	Tag   string   `json:"tag"`
	Rules []string `json:"rules"`
}

// ABFPolicy is an emulated ABF policy together with its attachment.
type ABFPolicy struct {
	ID       int    `json:"id"`
	ACLID    int    `json:"acl_id"`
	Via      string `json:"via"`
	Family   string `json:"family"`
	Ingress  string `json:"ingress,omitempty"`
	Priority int    `json:"priority,omitempty"`
	Local    bool   `json:"local,omitempty"`
}

// NATStatic is an endpoint-independent NAT44 static mapping.
type NATStatic struct {
	ID       string `json:"id"`
	Internal string `json:"internal"`
	External string `json:"external"`
}

// NATPort is an endpoint-independent NAT44 port mapping.
type NATPort struct {
	ID           string `json:"id"`
	Protocol     string `json:"protocol"`
	InternalHost string `json:"internal_host"`
	InternalPort int    `json:"internal_port"`
	External     string `json:"external"`
	ExternalPort int    `json:"external_port"`
}

// LCPPair is a Linux control-plane tap pair.
type LCPPair struct {
	VPP  string `json:"vpp"`
	Host string `json:"host"`
}

// PreNATRule is a native pre-NAT classifier rule.
type PreNATRule struct {
	ID       int    `json:"id"`
	Priority int    `json:"priority"`
	Prefixes int    `json:"prefixes"`
	Table    int    `json:"table"`
	FibIndex int    `json:"fib_index"`
	SkipNAT  bool   `json:"skip_nat"`
	Bypass   bool   `json:"bypass"`
	Source   string `json:"source,omitempty"`
	Dest     string `json:"dest,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Sport    string `json:"sport,omitempty"`
	Dport    string `json:"dport,omitempty"`
}

// PreNAT is the emulated native pre-NAT classifier inventory.
type PreNAT struct {
	Enabled   bool         `json:"enabled"`
	Interface string       `json:"interface,omitempty"`
	LANPrefix string       `json:"lan_prefix,omitempty"`
	Rules     []PreNATRule `json:"rules,omitempty"`
}

// DNSIntercept is the emulated transparent DNS interception state.
type DNSIntercept struct {
	Enabled   bool   `json:"enabled"`
	Interface string `json:"interface,omitempty"`
	Table     int    `json:"table,omitempty"`
}

// State is the complete emulated device state. It is JSON-serializable so a
// process-per-command fake keeps one coherent data plane across invocations.
type State struct {
	Interfaces   []*Interface     `json:"interfaces,omitempty"`
	Bonds        []*Bond          `json:"bonds,omitempty"`
	Tables       []*Table         `json:"tables,omitempty"`
	ACLs         []*ACL           `json:"acls,omitempty"`
	ABFs         []*ABFPolicy     `json:"abfs,omitempty"`
	NATStatic    []NATStatic      `json:"nat_static,omitempty"`
	NATPorts     []NATPort        `json:"nat_ports,omitempty"`
	NATAddresses []string         `json:"nat_addresses,omitempty"`
	QoSMaps      map[string][]int `json:"qos_maps,omitempty"`
	LCP          []LCPPair        `json:"lcp,omitempty"`
	Taps         []string         `json:"taps,omitempty"`
	PreNAT       PreNAT           `json:"pre_nat"`
	DNS          DNSIntercept     `json:"dns"`
	Policers     []string         `json:"policers,omitempty"`
	FlowRates    []string         `json:"flow_rates,omitempty"`
	GuardRules   []string         `json:"security_guard,omitempty"`
	NextACL      int              `json:"next_acl"`
	NextIfIndex  int              `json:"next_if_index"`
	Broadcast    bool             `json:"broadcast_local"`
}

// NewState returns an empty, initialized device.
func NewState() *State {
	return &State{QoSMaps: map[string][]int{}, NextACL: 30000, NextIfIndex: 1}
}

// Load reads a state file, creating an empty state when the file is absent.
func Load(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	state := NewState()
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, state); err != nil {
			return nil, fmt.Errorf("decode fake VPP state %s: %w", path, err)
		}
	}
	if state.QoSMaps == nil {
		state.QoSMaps = map[string][]int{}
	}
	if state.NextACL == 0 {
		state.NextACL = 30000
	}
	if state.NextIfIndex == 0 {
		state.NextIfIndex = 1
	}
	return state, nil
}

// Save writes the state file atomically.
func (state *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// withLock runs mutate while holding a cross-process state lock.
func withLock(statePath string, mutate func(*State) error) error {
	lockPath := statePath + ".lock"
	deadline := time.Now().Add(10 * time.Second)
	for {
		handle, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_ = handle.Close()
			break
		}
		if !os.IsExist(err) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil && time.Since(info.ModTime()) > 30*time.Second {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for fake VPP state lock %s", lockPath)
		}
		time.Sleep(2 * time.Millisecond)
	}
	defer os.Remove(lockPath)
	state, err := Load(statePath)
	if err != nil {
		return err
	}
	if err := mutate(state); err != nil {
		return err
	}
	return state.Save(statePath)
}

// stableID mirrors vpp.stableID so objects the fake synthesizes (for example
// NAT return-path guards) carry the identity the product will read back.
func stableID(value string, minimum, span int) int {
	if span <= 0 {
		return minimum
	}
	digest := sha256.Sum256([]byte(strings.TrimSpace(value)))
	return minimum + int(binary.BigEndian.Uint32(digest[:4])%uint32(span))
}

// safeTag mirrors vpp.safeTag.
func safeTag(value string) string {
	var builder strings.Builder
	for _, character := range strings.ToLower(strings.TrimSpace(value)) {
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			builder.WriteRune(character)
			continue
		}
		if builder.Len() > 0 {
			builder.WriteByte('_')
		}
	}
	result := strings.Trim(builder.String(), "_")
	if result == "" {
		return "default"
	}
	return result
}

func (state *State) interfaceByName(name string) *Interface {
	for _, item := range state.Interfaces {
		if item.Name == name {
			return item
		}
	}
	return nil
}

func (state *State) interfaceObject(name string) (interface{}, bool) {
	if item := state.interfaceByName(name); item != nil {
		return item, true
	}
	for _, bond := range state.Bonds {
		if bond.Name == name {
			return bond, true
		}
	}
	return nil, false
}

func (state *State) ensureInterface(name string) *Interface {
	if item := state.interfaceByName(name); item != nil {
		return item
	}
	item := &Interface{Name: name, Index: state.NextIfIndex, Hardware: true}
	state.NextIfIndex++
	state.Interfaces = append(state.Interfaces, item)
	return item
}

func (state *State) nextACLID() int {
	for {
		id := state.NextACL
		state.NextACL++
		if state.aclByID(id) == nil {
			return id
		}
	}
}

func (state *State) aclByID(id int) *ACL {
	for _, item := range state.ACLs {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func (state *State) removeACL(id int) {
	kept := state.ACLs[:0]
	for _, item := range state.ACLs {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	state.ACLs = kept
	for _, item := range state.Interfaces {
		item.InputACLs = removeInt(item.InputACLs, id)
		item.OutputACLs = removeInt(item.OutputACLs, id)
	}
}

func removeInt(values []int, unwanted int) []int {
	kept := values[:0]
	for _, value := range values {
		if value != unwanted {
			kept = append(kept, value)
		}
	}
	return kept
}

func (state *State) abfByID(id int) *ABFPolicy {
	for _, item := range state.ABFs {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func (state *State) removeABF(id int) {
	kept := state.ABFs[:0]
	for _, item := range state.ABFs {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	state.ABFs = kept
}

func (state *State) tableByID(id int) *Table {
	for _, item := range state.Tables {
		if item.ID == id {
			return item
		}
	}
	return nil
}

func (state *State) ensureTable(id int) *Table {
	if item := state.tableByID(id); item != nil {
		return item
	}
	item := &Table{ID: id, FibIndex: 4 + len(state.Tables)}
	state.Tables = append(state.Tables, item)
	return item
}

func (state *State) removeTable(id int) {
	kept := state.Tables[:0]
	for _, item := range state.Tables {
		if item.ID != id {
			kept = append(kept, item)
		}
	}
	state.Tables = kept
}

func (state *State) qosMap(id int) []int {
	if state.QoSMaps == nil {
		state.QoSMaps = map[string][]int{}
	}
	key := fmt.Sprintf("%d", id)
	values, found := state.QoSMaps[key]
	if !found || len(values) != 256 {
		values = make([]int, 256)
		state.QoSMaps[key] = values
	}
	return values
}

func (state *State) qosMapExists(id int) bool {
	if state.QoSMaps == nil {
		return false
	}
	_, found := state.QoSMaps[fmt.Sprintf("%d", id)]
	return found
}

// NATGuardFor computes the identity of the return-path guard a NAT mapping
// owns. The real product installs it through the NAT44 lifecycle; the fake
// models the same invariant so readback stays meaningful for both the raw and
// the dynamic client paths.
func NATGuardFor(id string) (policyID int, tag string) {
	return stableID("nat-return-abf:"+id, 20000, 8999), "ly-route-" + safeTag("nat-return-"+id)
}
