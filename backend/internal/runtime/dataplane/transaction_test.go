package dataplane

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

func TestDPDKTransactionAppliesOnlyAfterReadback(t *testing.T) {
	host := &fakeHost{}
	transaction := Transaction{Host: host, Now: fixedClock}

	receipt, err := transaction.Apply(context.Background(), dpdkRequest("txn-1"))

	if err != nil {
		t.Fatal(err)
	}
	want := []string{"snapshot", "stop-vpp", "configure-dpdk", "start-vpp", "verify-dpdk"}
	if !reflect.DeepEqual(host.trace, want) || receipt.Status != "applied" || receipt.Tier != vpp.DataplaneTierDPDK || receipt.Attachments != 1 || !receipt.Changed {
		t.Fatalf("trace=%v receipt=%#v", host.trace, receipt)
	}
}

func TestDPDKTransactionReusesIdenticalDeviceWidePathWithoutRestart(t *testing.T) {
	host := &fakeHost{}
	transaction := Transaction{Host: host, Now: fixedClock}
	if _, err := transaction.Apply(context.Background(), dpdkRequest("txn-first")); err != nil {
		t.Fatal(err)
	}
	traceAfterFirst := append([]string(nil), host.trace...)
	receipt, err := transaction.Apply(context.Background(), dpdkRequest("txn-second"))
	if err != nil {
		t.Fatal(err)
	}
	wantTrace := append(append([]string(nil), traceAfterFirst...), "verify-dpdk")
	if receipt.Status != "already_applied" || receipt.Changed || !reflect.DeepEqual(host.trace, wantTrace) {
		t.Fatalf("receipt=%#v trace=%v, want unchanged dataplane with verification %v", receipt, host.trace, wantTrace)
	}
	if _, err := transaction.Rollback(context.Background(), "txn-second"); err == nil {
		t.Fatal("reused path unexpectedly owned a rollback snapshot")
	}
}

func TestDPDKTransactionReusesPersistedPathAfterControlPlaneRestart(t *testing.T) {
	host := &fakeHost{}
	first := Transaction{Host: host, Now: fixedClock}
	if _, err := first.Apply(context.Background(), dpdkRequest("txn-first")); err != nil {
		t.Fatal(err)
	}
	traceAfterFirst := append([]string(nil), host.trace...)
	restarted := Transaction{Host: host, Now: fixedClock}
	receipt, err := restarted.Apply(context.Background(), dpdkRequest("txn-after-restart"))
	if err != nil {
		t.Fatal(err)
	}
	wantTrace := append(append([]string(nil), traceAfterFirst...), "verify-dpdk")
	if receipt.Status != "already_applied" || receipt.Changed || !reflect.DeepEqual(host.trace, wantTrace) {
		t.Fatalf("receipt=%#v trace=%v, want persisted path verification", receipt, host.trace)
	}
}

func TestDPDKTransactionReappliesPersistedPathWhenVPPRuntimeIsStale(t *testing.T) {
	host := &fakeHost{}
	first := Transaction{Host: host, Now: fixedClock}
	if _, err := first.Apply(context.Background(), dpdkRequest("txn-first")); err != nil {
		t.Fatal(err)
	}
	host.verifyDPDKFailures = 1
	restarted := Transaction{Host: host, Now: fixedClock}
	receipt, err := restarted.Apply(context.Background(), dpdkRequest("txn-replay"))
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "applied" || !receipt.Changed {
		t.Fatalf("receipt=%#v, want re-applied path", receipt)
	}
	wantSuffix := []string{"verify-dpdk", "snapshot", "stop-vpp", "configure-dpdk", "start-vpp", "verify-dpdk"}
	if !reflect.DeepEqual(host.trace[len(host.trace)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("trace=%v, want suffix=%v", host.trace, wantSuffix)
	}
}

func TestDPDKTransactionReconcilesWidenedDeviceWidePath(t *testing.T) {
	host := &fakeHost{}
	transaction := Transaction{Host: host, Now: fixedClock}
	if _, err := transaction.Apply(context.Background(), dpdkRequest("txn-lan")); err != nil {
		t.Fatal(err)
	}
	host.trace = nil

	widened := dpdkRequest("txn-lan-wan")
	widened.Path.Attachments = append(widened.Path.Attachments,
		vpp.NativeAttachment{LinuxInterface: "eth2", VPPInterface: "lyroute-eth2", Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: "0000:04:00.0", KernelDriver: "igb", IOMMUGroup: "none"})
	receipt, err := transaction.Apply(context.Background(), widened)
	if err != nil {
		t.Fatalf("a WAN port added after LAN must not be reported as a path conflict: %v", err)
	}
	want := []string{"snapshot", "stop-vpp", "configure-dpdk", "start-vpp", "verify-dpdk"}
	if receipt.Status != "applied" || !receipt.Changed || receipt.Attachments != 2 || !reflect.DeepEqual(host.trace, want) {
		t.Fatalf("receipt=%#v trace=%v, want a new generation %v", receipt, host.trace, want)
	}
	if len(host.snapshotRequests) != 2 || len(host.snapshotRequests[1].Superseded) != 1 || host.snapshotRequests[1].Superseded[0].LinuxInterface != "eth1" {
		t.Fatalf("snapshot requests=%#v, want the superseded LAN port carried into the rollback snapshot", host.snapshotRequests)
	}
	if host.verifiedPaths[1].Attachments[1].LinuxInterface != "eth2" {
		t.Fatalf("verified path=%#v, want the widened path", host.verifiedPaths)
	}
}

func TestSameNativePathIgnoresProofBookkeepingAndOrder(t *testing.T) {
	left := dpdkRequest("txn-left").Path
	right := dpdkRequest("txn-right").Path
	right.Attachments[0].MACAddress = "52:54:00:00:00:99"
	right.Attachments[0].VPPInterface = "GigabitEthernet3/0/0"
	right.Attachments[0].HardwareVPPInterface = "GigabitEthernet3/0/0"
	right.Attachments[0].Verification = "pending_link"
	right.Attachments[0].IOMMUGroup = ""
	if !SameNativePath(left, right) {
		t.Fatalf("proof-only differences forced a path conflict: %#v vs %#v", left, right)
	}
	// Older generations persisted the short transport spelling.
	legacy := dpdkRequest("txn-legacy").Path
	legacy.Attachments[0].Mode = "vfio"
	if !SameNativePath(left, legacy) {
		t.Fatal("legacy vfio mode spelling was not recognised as the same binding")
	}
	// Attachment order is not identity.
	two := dpdkRequest("txn-two").Path
	two.Attachments = append(two.Attachments,
		vpp.NativeAttachment{LinuxInterface: "eth2", VPPInterface: "lyroute-eth2", Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKUIO, PCIAddress: "0000:04:00.0", KernelDriver: "igb", IOMMUGroup: "none"})
	reversed := two
	reversed.Attachments = []vpp.NativeAttachment{two.Attachments[1], two.Attachments[0]}
	if !SameNativePath(two, reversed) {
		t.Fatal("attachment order changed path identity")
	}
	// A different port is a different path.
	different := dpdkRequest("txn-different").Path
	different.Attachments[0].PCIAddress = "0000:09:00.0"
	different.Attachments[0].LinuxInterface = "eth9"
	if SameNativePath(left, different) {
		t.Fatal("a different port was accepted as the same path")
	}
}

func TestDPDKTransactionRestoresSnapshotOnEveryPostStopFailure(t *testing.T) {
	for _, phase := range []string{"configure-dpdk", "start-vpp", "verify-dpdk"} {
		t.Run(phase, func(t *testing.T) {
			host := &fakeHost{fail: phase}
			_, err := (&Transaction{Host: host, Now: fixedClock}).Apply(context.Background(), dpdkRequest("txn-fail"))
			if !errors.Is(err, ErrTransactionFailed) || len(host.trace) == 0 || host.trace[len(host.trace)-1] != "restore" {
				t.Fatalf("error=%v trace=%v", err, host.trace)
			}
		})
	}
}

func TestDPDKCommittedTransactionCanBeRolledBack(t *testing.T) {
	host := &fakeHost{}
	transaction := Transaction{Host: host, Now: fixedClock}
	if _, err := transaction.Apply(context.Background(), dpdkRequest("txn-rollback")); err != nil {
		t.Fatal(err)
	}
	receipt, err := transaction.Rollback(context.Background(), "txn-rollback")
	if err != nil || !receipt.RolledBack || host.trace[len(host.trace)-1] != "restore" {
		t.Fatalf("error=%v receipt=%#v trace=%v", err, receipt, host.trace)
	}
}

func TestNativeTierDoesNotTouchPrivilegedHost(t *testing.T) {
	host := &fakeHost{}
	request := Request{TransactionID: "txn-native", Path: vpp.NativePath{Tier: vpp.DataplaneTierNative, Attachments: []vpp.NativeAttachment{{LinuxInterface: "eth1"}}}}
	if _, err := (&Transaction{Host: host, Now: fixedClock}).Apply(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(host.trace) != 0 {
		t.Fatalf("privileged trace = %v", host.trace)
	}
}

type fakeHost struct {
	trace              []string
	fail               string
	verifyDPDKFailures int
	active             *ActiveState
	snapshotRequests   []Request
	verifiedPaths      []vpp.NativePath
}

func (host *fakeHost) call(name string) error {
	host.trace = append(host.trace, name)
	if host.fail == name {
		return errors.New("injected " + name + " failure")
	}
	return nil
}

func (host *fakeHost) Snapshot(_ context.Context, request Request) (Snapshot, error) {
	host.snapshotRequests = append(host.snapshotRequests, request)
	err := host.call("snapshot")
	return Snapshot{TransactionID: request.TransactionID, Devices: []DeviceState{{LinuxInterface: "eth1", PCIAddress: "0000:03:00.0", KernelDriver: "ixgbe", LinkUp: true}}}, err
}
func (host *fakeHost) StopVPP(context.Context) error { return host.call("stop-vpp") }
func (host *fakeHost) ConfigureDPDK(context.Context, vpp.NativePath, Snapshot) error {
	return host.call("configure-dpdk")
}
func (host *fakeHost) StartVPP(context.Context) error { return host.call("start-vpp") }
func (host *fakeHost) VerifyDPDK(_ context.Context, path vpp.NativePath) error {
	host.verifiedPaths = append(host.verifiedPaths, path)
	if host.verifyDPDKFailures > 0 {
		host.verifyDPDKFailures--
		host.trace = append(host.trace, "verify-dpdk")
		return errors.New("injected stale runtime")
	}
	return host.call("verify-dpdk")
}
func (host *fakeHost) Restore(context.Context, Snapshot) error { return host.call("restore") }
func (host *fakeHost) LoadActiveState(context.Context) (ActiveState, bool, error) {
	if host.active == nil {
		return ActiveState{}, false, nil
	}
	return *host.active, true, nil
}
func (host *fakeHost) SaveActiveState(_ context.Context, state ActiveState) error {
	host.active = &state
	return nil
}
func (host *fakeHost) ClearActiveState(context.Context) error {
	host.active = nil
	return nil
}

func dpdkRequest(id string) Request {
	return Request{TransactionID: id, Path: vpp.NativePath{Tier: vpp.DataplaneTierDPDK, Attachments: []vpp.NativeAttachment{{LinuxInterface: "eth1", VPPInterface: "lyroute-eth1", Tier: vpp.DataplaneTierDPDK, Hook: vpp.NativeHookDPDK, Mode: vpp.NativeModeDPDKVFIO, PCIAddress: "0000:03:00.0", KernelDriver: "ixgbe", IOMMUGroup: "17"}}}}
}

func fixedClock() time.Time { return time.Date(2026, 7, 30, 14, 0, 0, 0, time.UTC) }
