package httpapi

// Ownership and verification records outlive the hardware they describe. The
// interface inventory already drops entries whose PCI device disappeared, but
// ownership.json and dpdk-verification.json kept probe records pointed at a
// device that is no longer on the bus (an unplugged USB NIC, a port moved to
// another slot). Those stale records made the watcher and the take-over report
// describe hardware that cannot be opened.

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/dataplane"
)

// devicePresent reports whether the PCI address still exists on this host. An
// empty address cannot be judged, so the record is kept rather than guessed away.
func devicePresent(pci string) bool {
	pci = strings.TrimSpace(pci)
	if pci == "" {
		return true
	}
	_, err := os.Stat(filepath.Join(sysfsRoot(), "bus", "pci", "devices", pci))
	return err == nil
}

// pruneDataplaneFacts returns how many ownership and verification records were
// dropped because their hardware is gone.
func (server *Server) pruneDataplaneFacts() (int, int) {
	removedOwnership := pruneOwnershipFacts(dataplaneOwnershipPath())
	removedVerification := pruneVerificationFacts(dataplaneVerificationPath())
	if removedOwnership > 0 || removedVerification > 0 {
		log.Printf("dataplane facts: pruned %d ownership and %d verification records for removed hardware", removedOwnership, removedVerification)
	}
	return removedOwnership, removedVerification
}

func pruneOwnershipFacts(path string) int {
	state, err := dataplane.LoadOwnershipState(path)
	if err != nil || len(state.Devices) == 0 {
		return 0
	}
	kept := make([]dataplane.OwnershipDevice, 0, len(state.Devices))
	removed := 0
	for _, device := range state.Devices {
		if devicePresent(device.PCIAddress) {
			kept = append(kept, device)
			continue
		}
		removed++
	}
	if removed == 0 {
		return 0
	}
	state.Devices = kept
	state.UpdatedAt = time.Now().UTC()
	if err := writeJSONAtomic(path, state); err != nil {
		log.Printf("dataplane facts: rewrite %s: %v", path, err)
		return 0
	}
	return removed
}

func pruneVerificationFacts(path string) int {
	state, err := dataplane.LoadDPDKVerification(path)
	if err != nil || len(state.Interfaces) == 0 {
		return 0
	}
	kept := make([]dataplane.DPDKVerification, 0, len(state.Interfaces))
	removed := 0
	for _, entry := range state.Interfaces {
		if devicePresent(entry.PCIAddress) {
			kept = append(kept, entry)
			continue
		}
		removed++
	}
	if removed == 0 {
		return 0
	}
	state.Interfaces = kept
	state.UpdatedAt = time.Now().UTC()
	if err := dataplane.SaveDPDKVerification(path, state); err != nil {
		log.Printf("dataplane facts: rewrite %s: %v", path, err)
		return 0
	}
	return removed
}

// writeJSONAtomic replaces a state file without exposing a half-written file to
// the boot preflight, which reads these records before the control plane starts.
func writeJSONAtomic(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, payload, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
