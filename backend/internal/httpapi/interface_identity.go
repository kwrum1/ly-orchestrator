package httpapi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"ly-route/backend/internal/runtime/dataplane"
)

// Interface ids are product identities, not kernel netdev names. They must be
// stable from the first observation and must not change when a NIC is bound to
// VFIO/UIO (which removes the Linux netdev). The ordering is derived from the
// stable PCI address, with the management port pinned to eth0.
type interfaceIdentity struct {
	ID             string
	LinuxInterface string
	PCI            string
	MAC            string
	Management     bool
}

var pciAddressPattern = regexp.MustCompile(`^[0-9A-Fa-f]{4}:[0-9A-Fa-f]{2}:[0-9A-Fa-f]{2}\.[0-7]$`)

type interfaceIdentityCandidate struct {
	linux string
	pci   string
	mac   string
}

func normalizePCICandidate(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if !pciAddressPattern.MatchString(value) {
		return ""
	}
	return value
}

func normalizeMACCandidate(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func (candidate *interfaceIdentityCandidate) key() string {
	switch {
	case candidate.pci != "":
		return "pci:" + candidate.pci
	case candidate.mac != "":
		return "mac:" + candidate.mac
	case candidate.linux != "":
		return "name:" + candidate.linux
	default:
		return ""
	}
}

// interfaceIdentities builds the canonical identity list from the union of the
// current kernel inventory and the persisted ownership evidence. The union is
// what makes the ids stable across a DPDK bind: after the netdev disappears,
// the persisted inventory/ownership record still carries the same PCI address.
func (server *Server) interfaceIdentities(ctx context.Context) []interfaceIdentity {
	if cache := interfaceIdentityCacheFromContext(ctx); cache != nil {
		return cache.snapshot(server, ctx)
	}
	return server.buildInterfaceIdentities(ctx)
}

func (server *Server) buildInterfaceIdentities(ctx context.Context) []interfaceIdentity {
	managementName := strings.TrimSpace(server.managementInterfaceID(ctx))
	candidates := map[string]*interfaceIdentityCandidate{}
	add := func(linux, pci, mac string) {
		linux = strings.TrimSpace(linux)
		pci = normalizePCICandidate(pci)
		mac = normalizeMACCandidate(mac)
		if linux == "" && pci == "" && mac == "" {
			return
		}
		candidate := &interfaceIdentityCandidate{linux: linux, pci: pci, mac: mac}
		key := candidate.key()
		if key == "" {
			return
		}
		if existing, ok := candidates[key]; ok {
			if existing.linux == "" {
				existing.linux = linux
			}
			if existing.pci == "" {
				existing.pci = pci
			}
			if existing.mac == "" {
				existing.mac = mac
			}
			return
		}
		candidates[key] = candidate
	}

	if managementName != "" {
		add(managementName, sysfsPCI(managementName), sysfsMAC(managementName))
	}
	for _, item := range hostInterfaceInventory() {
		name := strings.TrimSpace(firstNonEmpty(stringField(item, "system_name"), stringField(item, "id"), stringField(item, "name")))
		add(name, stringField(item, "pci"), stringField(item, "mac"))
	}
	// Persisted inventory is only authoritative for a port that is currently
	// owned destructively (VFIO/UIO) and therefore has no kernel netdev. A
	// stale inventory file must not add ports that this boot never observed.
	ownedPCI := map[string]bool{}
	ownedName := map[string]bool{}
	if ownership, err := dataplane.LoadOwnershipState(dataplaneOwnershipPath()); err == nil {
		for _, device := range ownership.Devices {
			add(device.LinuxInterface, device.PCIAddress, device.MACAddress)
			if pci := normalizePCICandidate(device.PCIAddress); pci != "" {
				ownedPCI[pci] = true
			}
			if name := strings.TrimSpace(device.LinuxInterface); name != "" {
				ownedName[name] = true
			}
		}
	}
	if data, err := os.ReadFile(envOrDefault("LY_ROUTE_DATAPLANE_ACTIVE_STATE", "/var/lib/ly-route/dataplane/active.json")); err == nil {
		var active activeDataplaneFile
		if json.Unmarshal(data, &active) == nil {
			for _, attachment := range active.Path.Attachments {
				add(attachment.LinuxInterface, attachment.PCIAddress, "")
				if pci := normalizePCICandidate(attachment.PCIAddress); pci != "" {
					ownedPCI[pci] = true
				}
				if name := strings.TrimSpace(attachment.LinuxInterface); name != "" {
					ownedName[name] = true
				}
			}
		}
	}
	if inventory, err := dataplane.LoadInventory(dataplaneInventoryPath()); err == nil {
		for _, item := range inventory.DataInterfaces {
			pci := normalizePCICandidate(item.PCIAddress)
			if ownedPCI[pci] || ownedName[strings.TrimSpace(item.LinuxInterface)] || (pci != "" && ownedPCI[pci]) {
				add(item.LinuxInterface, item.PCIAddress, item.MACAddress)
			}
		}
	}

	managementKey := ""
	if managementName != "" {
		if candidate, ok := candidates["pci:"+sysfsPCI(managementName)]; ok && candidate != nil {
			managementKey = candidate.key()
		} else if candidate, ok := candidates["name:"+managementName]; ok && candidate != nil {
			managementKey = candidate.key()
		}
	}
	result := make([]interfaceIdentity, 0, len(candidates)+1)
	if managementKey != "" {
		candidate := candidates[managementKey]
		result = append(result, interfaceIdentity{
			ID:             "eth0",
			LinuxInterface: candidate.linux,
			PCI:            candidate.pci,
			MAC:            candidate.mac,
			Management:     true,
		})
	} else if managementName != "" {
		result = append(result, interfaceIdentity{
			ID:             "eth0",
			LinuxInterface: managementName,
			PCI:            sysfsPCI(managementName),
			MAC:            sysfsMAC(managementName),
			Management:     true,
		})
	}
	dataCandidates := make([]*interfaceIdentityCandidate, 0, len(candidates))
	for key, candidate := range candidates {
		if key == managementKey {
			continue
		}
		if managementName != "" && candidate.linux == managementName {
			continue
		}
		dataCandidates = append(dataCandidates, candidate)
	}
	sort.SliceStable(dataCandidates, func(left, right int) bool {
		leftPCI, rightPCI := dataCandidates[left].pci, dataCandidates[right].pci
		if leftPCI != rightPCI {
			if leftPCI == "" {
				return false
			}
			if rightPCI == "" {
				return true
			}
			return leftPCI < rightPCI
		}
		if dataCandidates[left].mac != dataCandidates[right].mac {
			return dataCandidates[left].mac < dataCandidates[right].mac
		}
		return dataCandidates[left].linux < dataCandidates[right].linux
	})
	for index, candidate := range dataCandidates {
		result = append(result, interfaceIdentity{
			ID:             "eth" + strconv.Itoa(index+1),
			LinuxInterface: candidate.linux,
			PCI:            candidate.pci,
			MAC:            candidate.mac,
		})
	}
	return result
}

func sysfsPCI(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot(), "class/net", name, "device"))
	if err != nil {
		return ""
	}
	return normalizePCICandidate(filepath.Base(resolved))
}

func sysfsMAC(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(sysfsRoot(), "class/net", name, "address"))
	if err != nil {
		return ""
	}
	return normalizeMACCandidate(string(data))
}

// canonicalInterfaceID maps a product id, Linux netdev name, PCI address or MAC
// to the stable product id. Unknown identifiers are returned unchanged so a
// policy for a not-yet-present port is not silently dropped.
func (server *Server) canonicalInterfaceID(ctx context.Context, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	identities := server.interfaceIdentities(ctx)
	for _, identity := range identities {
		if strings.EqualFold(identity.ID, value) {
			return identity.ID
		}
	}
	lower := strings.ToLower(value)
	for _, identity := range identities {
		if lower == strings.ToLower(identity.LinuxInterface) || lower == strings.ToLower(identity.PCI) || lower == strings.ToLower(identity.MAC) {
			return identity.ID
		}
	}
	if pci := sysfsPCI(value); pci != "" {
		for _, identity := range identities {
			if identity.PCI == pci {
				return identity.ID
			}
		}
	}
	if mac := sysfsMAC(value); mac != "" {
		for _, identity := range identities {
			if identity.MAC == mac {
				return identity.ID
			}
		}
	}
	return value
}

// canonicalAFRequiredInterfaces turns the selection layer's Linux names into
// product ids for the 409 af_fallback_required payload.
func (server *Server) canonicalAFRequiredInterfaces(ctx context.Context, names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		port := server.canonicalInterfaceID(ctx, name)
		if port == "" {
			port = strings.TrimSpace(name)
		}
		if port == "" || containsString(result, port) {
			continue
		}
		result = append(result, port)
	}
	return result
}

type interfaceIdentityCacheKey struct{}

type interfaceIdentityCache struct {
	once  sync.Once
	items []interfaceIdentity
}

func withInterfaceIdentities(ctx context.Context) context.Context {
	if interfaceIdentityCacheFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, interfaceIdentityCacheKey{}, &interfaceIdentityCache{})
}

func interfaceIdentityCacheFromContext(ctx context.Context) *interfaceIdentityCache {
	if ctx == nil {
		return nil
	}
	cache, _ := ctx.Value(interfaceIdentityCacheKey{}).(*interfaceIdentityCache)
	return cache
}

func (cache *interfaceIdentityCache) snapshot(server *Server, ctx context.Context) []interfaceIdentity {
	cache.once.Do(func() {
		cache.items = server.buildInterfaceIdentities(ctx)
	})
	return cache.items
}
