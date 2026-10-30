package dataplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

var ErrTransactionFailed = errors.New("dataplane transaction failed")

type DeviceState struct {
	LinuxInterface string `json:"linux_interface"`
	PCIAddress     string `json:"pci_address"`
	// KernelDriver is the Linux driver the device must return to on rollback.
	KernelDriver string `json:"kernel_driver"`
	// BoundDriver is the driver the device was actually bound to when the
	// snapshot was taken. It differs from KernelDriver when the boot ownership
	// preflight already handed the NIC to VPP.
	BoundDriver    string `json:"bound_driver,omitempty"`
	OwnedByProduct bool   `json:"owned_by_product,omitempty"`
	// PreviousDPDK marks a device that belonged to the device-wide path that
	// is being replaced. Rollback restores its DPDK binding; devices that are
	// new to this generation are returned to the kernel.
	PreviousDPDK bool `json:"previous_dpdk,omitempty"`
	LinkUp       bool `json:"link_up"`
}

type Snapshot struct {
	TransactionID string        `json:"transaction_id"`
	StartupConfig []byte        `json:"startup_config"`
	Devices       []DeviceState `json:"devices"`
}

type Receipt struct {
	TransactionID string            `json:"transaction_id"`
	Tier          vpp.DataplaneTier `json:"tier"`
	Status        string            `json:"status"`
	AppliedAt     time.Time         `json:"applied_at"`
	Attachments   int               `json:"attachments"`
	RolledBack    bool              `json:"rolled_back,omitempty"`
	Changed       bool              `json:"changed"`
}

type Request struct {
	TransactionID string
	Path          vpp.NativePath
	// Superseded carries the currently active attachments when the requested
	// device-wide path adds or removes ports (for example LAN committed first,
	// WAN assigned later). The host snapshots those devices too so a failed
	// generation can restore the previous binding instead of leaving a port
	// half-owned by DPDK.
	Superseded []vpp.NativeAttachment
}

type ActiveState struct {
	Path      vpp.NativePath `json:"path"`
	Snapshot  Snapshot       `json:"snapshot"`
	AppliedAt time.Time      `json:"applied_at"`
}

type ActiveStateStore interface {
	LoadActiveState(context.Context) (ActiveState, bool, error)
	SaveActiveState(context.Context, ActiveState) error
	ClearActiveState(context.Context) error
}

type DHCPServiceRebinder interface {
	RebindDHCPService(context.Context) error
}

// Host owns privileged Linux/VPP operations. Its implementation must make
// ConfigureDPDK atomic and Restore idempotent so recovery is safe to repeat.
type Host interface {
	Snapshot(context.Context, Request) (Snapshot, error)
	StopVPP(context.Context) error
	ConfigureDPDK(context.Context, vpp.NativePath, Snapshot) error
	StartVPP(context.Context) error
	VerifyDPDK(context.Context, vpp.NativePath) error
	Restore(context.Context, Snapshot) error
}

// DPDKRebuildVerifier is implemented by hosts that can tell a port this
// transaction just rebuilt (still inside VPP's delete/create/rename window)
// apart from a port that is genuinely missing.
type DPDKRebuildVerifier interface {
	VerifyRebuiltDPDK(context.Context, vpp.NativePath) error
}

type Transaction struct {
	Host Host
	Now  func() time.Time

	mu          sync.Mutex
	active      map[string]Snapshot
	current     *vpp.NativePath
	initialized bool
}

func (transaction *Transaction) RebindDHCPService(ctx context.Context) error {
	rebinder, ok := transaction.Host.(DHCPServiceRebinder)
	if !ok {
		return nil
	}
	return rebinder.RebindDHCPService(ctx)
}

func (transaction *Transaction) Apply(ctx context.Context, request Request) (Receipt, error) {
	if request.TransactionID == "" {
		return Receipt{}, fmt.Errorf("%w: transaction ID is required", ErrTransactionFailed)
	}
	if request.Path.Tier == vpp.DataplaneTierNative {
		transaction.mu.Lock()
		defer transaction.mu.Unlock()
		if err := transaction.loadCurrent(ctx); err != nil {
			return Receipt{}, fmt.Errorf("%w: load active state: %w", ErrTransactionFailed, err)
		}
		if transaction.current != nil {
			if !SameNativePath(*transaction.current, request.Path) {
				if request.Path.Tier == vpp.DataplaneTierNative && !hasDPDKAttachment(transaction.current.Attachments) {
					// A native request never binds a PCI device and this branch
					// has no side effects, so it cannot conflict with a committed
					// device-wide path. Reaching here with a stale DPDK
					// active.json (an earlier experiment whose device is back on
					// its kernel driver) used to reject every apply with
					// "requested native path differs from the active device-wide
					// path"; the takeover engine owns the actual move.
					transaction.current = nil
					return transaction.receipt(request, false), nil
				}
				return Receipt{}, fmt.Errorf("%w: requested native path differs from the active device-wide path", ErrTransactionFailed)
			}
			receipt := transaction.receipt(request, false)
			receipt.Status = "already_applied"
			return receipt, nil
		}
		return transaction.receipt(request, false), nil
	}
	if request.Path.Tier != vpp.DataplaneTierDPDK || len(request.Path.Attachments) == 0 {
		return Receipt{}, fmt.Errorf("%w: unsupported or empty tier %q", ErrTransactionFailed, request.Path.Tier)
	}
	if transaction.Host == nil {
		return Receipt{}, fmt.Errorf("%w: privileged host adapter is unavailable", ErrTransactionFailed)
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if err := transaction.loadCurrent(ctx); err != nil {
		return Receipt{}, fmt.Errorf("%w: load active state: %w", ErrTransactionFailed, err)
	}
	if transaction.current != nil {
		if SameNativePath(*transaction.current, request.Path) {
			if err := transaction.Host.VerifyDPDK(ctx, request.Path); err == nil {
				receipt := transaction.receipt(request, false)
				receipt.Status = "already_applied"
				return receipt, nil
			}
			// active.json survives a control-plane restart, while VPP itself may
			// have restarted with an empty graph. Reapply the persisted path so the
			// subsequent gateway transaction sees real hardware interfaces.
			transaction.current = nil
		} else {
			// The device-wide path legitimately changed (a WAN port was added to
			// an appliance that already committed its LAN, or a port was removed).
			// This is a new generation, not a conflict: apply it transactionally
			// and keep the previous attachments in the snapshot so a rollback
			// restores their original kernel/DPDK binding.
			request.Superseded = append([]vpp.NativeAttachment(nil), transaction.current.Attachments...)
		}
	}

	snapshot, err := transaction.Host.Snapshot(ctx, request)
	if err != nil {
		return Receipt{}, fmt.Errorf("%w: snapshot: %w", ErrTransactionFailed, err)
	}
	mutated := false
	fail := func(phase string, cause error) (Receipt, error) {
		if !mutated {
			return Receipt{}, fmt.Errorf("%w: %s: %w", ErrTransactionFailed, phase, cause)
		}
		rollbackErr := transaction.Host.Restore(ctx, snapshot)
		return Receipt{}, fmt.Errorf("%w: %s: %w", ErrTransactionFailed, phase, errors.Join(cause, rollbackErr))
	}
	if err := transaction.Host.StopVPP(ctx); err != nil {
		return fail("stop-vpp", err)
	}
	mutated = true
	if err := transaction.Host.ConfigureDPDK(ctx, request.Path, snapshot); err != nil {
		return fail("configure-dpdk", err)
	}
	if err := transaction.Host.StartVPP(ctx); err != nil {
		return fail("start-vpp", err)
	}
	if err := transaction.verifyDPDKAfterRebuild(ctx, request.Path); err != nil {
		return fail("readback", err)
	}
	if store, ok := transaction.Host.(ActiveStateStore); ok {
		if err := store.SaveActiveState(ctx, ActiveState{Path: request.Path, Snapshot: snapshot, AppliedAt: transaction.now()}); err != nil {
			return fail("persist-state", err)
		}
	}

	if transaction.active == nil {
		transaction.active = map[string]Snapshot{}
	}
	transaction.active[request.TransactionID] = snapshot
	current := request.Path
	transaction.current = &current
	receipt := transaction.receipt(request, false)
	receipt.Changed = true
	return receipt, nil
}

// verifyDPDKAfterRebuild prefers the bounded post-rebuild readback when the
// host can distinguish "still being re-created" from "missing". Hosts that
// cannot keep the exact previous behavior.
func (transaction *Transaction) verifyDPDKAfterRebuild(ctx context.Context, path vpp.NativePath) error {
	if verifier, ok := transaction.Host.(DPDKRebuildVerifier); ok {
		return verifier.VerifyRebuiltDPDK(ctx, path)
	}
	return transaction.Host.VerifyDPDK(ctx, path)
}

func (transaction *Transaction) Rollback(ctx context.Context, transactionID string) (Receipt, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	snapshot, found := transaction.active[transactionID]
	delete(transaction.active, transactionID)
	if !found {
		return Receipt{}, fmt.Errorf("%w: no active snapshot for %q", ErrTransactionFailed, transactionID)
	}
	if err := transaction.Host.Restore(ctx, snapshot); err != nil {
		return Receipt{}, fmt.Errorf("%w: rollback: %w", ErrTransactionFailed, err)
	}
	transaction.current = nil
	if store, ok := transaction.Host.(ActiveStateStore); ok {
		if err := store.ClearActiveState(ctx); err != nil {
			return Receipt{}, fmt.Errorf("%w: clear active state: %w", ErrTransactionFailed, err)
		}
	}
	tier := vpp.DataplaneTierDPDK
	if transaction.current != nil {
		tier = transaction.current.Tier
	}
	return Receipt{TransactionID: transactionID, Tier: tier, Status: "rolled_back", AppliedAt: transaction.now(), Attachments: len(snapshot.Devices), RolledBack: true, Changed: true}, nil
}

func (transaction *Transaction) loadCurrent(ctx context.Context) error {
	if transaction.initialized {
		return nil
	}
	store, ok := transaction.Host.(ActiveStateStore)
	if !ok {
		transaction.initialized = true
		return nil
	}
	state, found, err := store.LoadActiveState(ctx)
	if err != nil {
		return err
	}
	transaction.initialized = true
	if found {
		path := state.Path
		transaction.current = &path
	}
	return nil
}

// hasDPDKAttachment reports whether a path owns a PCI device destructively.
func hasDPDKAttachment(attachments []vpp.NativeAttachment) bool {
	for _, attachment := range attachments {
		if attachment.Tier == vpp.DataplaneTierDPDK {
			return true
		}
	}
	return false
}

func SameNativePath(left, right vpp.NativePath) bool {
	if left.Tier != right.Tier || left.SmartQoS != right.SmartQoS || len(left.Attachments) != len(right.Attachments) {
		return false
	}
	remaining := append([]vpp.NativeAttachment(nil), right.Attachments...)
	for _, a := range left.Attachments {
		match := -1
		for index, b := range remaining {
			if sameAttachmentIdentity(a, b) {
				match = index
				break
			}
		}
		if match < 0 {
			return false
		}
		remaining = append(remaining[:match], remaining[match+1:]...)
	}
	return len(remaining) == 0
}

// sameAttachmentIdentity compares only the fields that decide which port is
// attached and how it is bound. Attachment order and proof bookkeeping (MAC,
// verification state, VPP hardware name, IOMMU group text) are rewritten by
// every capability probe and must never force a re-bind or a path conflict.
func sameAttachmentIdentity(a, b vpp.NativeAttachment) bool {
	if a.Tier != b.Tier || a.Hook != b.Hook {
		return false
	}
	if normalizeNativeMode(a.Mode) != normalizeNativeMode(b.Mode) {
		return false
	}
	if strings.TrimSpace(a.LinuxInterface) != strings.TrimSpace(b.LinuxInterface) {
		return false
	}
	if a.PCIAddress != "" && b.PCIAddress != "" && !strings.EqualFold(a.PCIAddress, b.PCIAddress) {
		return false
	}
	// A DPDK port is identified by its Linux/PCI identity. Persisted active
	// state from before the product rename still carries VPP's hardware name;
	// it must not force a full re-bind just to rename.
	if a.Tier != vpp.DataplaneTierDPDK && a.VPPInterface != b.VPPInterface {
		return false
	}
	return true
}

// normalizeNativeMode folds the historical transport spellings into the mode
// values this build persists, so an older active generation is recognised as
// the same binding instead of being reported as a different DPDK path.
func normalizeNativeMode(mode vpp.NativeMode) string {
	switch strings.ToLower(strings.TrimSpace(string(mode))) {
	case "uio", "uio_pci_generic":
		return "uio_pci_generic"
	case "vfio", "vfio-pci", "vfio_pci":
		return "vfio_pci"
	}
	return strings.ToLower(strings.TrimSpace(string(mode)))
}

func (transaction *Transaction) receipt(request Request, rolledBack bool) Receipt {
	return Receipt{TransactionID: request.TransactionID, Tier: request.Path.Tier, Status: "applied", AppliedAt: transaction.now(), Attachments: len(request.Path.Attachments), RolledBack: rolledBack}
}

func (transaction *Transaction) now() time.Time {
	if transaction.Now == nil {
		return time.Now().UTC()
	}
	return transaction.Now().UTC()
}
