package takeover

import (
	"errors"
	"testing"
)

func TestClassifyFailureStages(t *testing.T) {
	cases := []struct {
		stage string
		err   error
		want  FailureClass
	}{
		{stage: "cleanup", err: errors.New("boom"), want: FailureCleanup},
		{stage: "bind", err: errors.New("cannot release 0000:03:00.0"), want: FailureCleanup},
		{stage: "bind", err: errors.New("Device or resource busy"), want: FailureBusy},
		{stage: "start", err: errors.New("VPP did not become ready"), want: FailureEnvironment},
		{stage: "bind", err: errors.New("EAL: no free hugepages"), want: FailureEnvironment},
		{stage: "start", err: errors.New("invalid pmd in startup.conf"), want: FailureConfig},
		{stage: "rx", err: errors.New("no packets"), want: FailureRX},
		{stage: "tx", err: errors.New("tx poll failed"), want: FailureTX},
		{stage: "verify", err: errors.New("carrier down"), want: FailureForward},
		{stage: "bind", err: errors.New("driver does not support this mode"), want: FailureUnsupported},
		{stage: "bind", err: errors.New("something odd"), want: FailureUnknown},
	}
	for _, testCase := range cases {
		if got := Classify(testCase.stage, testCase.err); got != testCase.want {
			t.Fatalf("Classify(%s, %v) = %s, want %s", testCase.stage, testCase.err, got, testCase.want)
		}
	}
}

func TestAdvanceFallsBackFromDPDKStraightToCompatibilityForIGC(t *testing.T) {
	plan := BuildPlan(igcFacts())
	next := Advance(plan, ModeDPDKUIO, FailureUnsupported)
	if next.Stop || next.NextMode != ModeAFPacket {
		t.Fatalf("after dpdk failure: %#v, want the AF_PACKET fallback", next)
	}
	next = Advance(plan, ModeAFPacket, FailureForward)
	if !next.Stop || next.NextMode != "" {
		t.Fatalf("after compatible-mode failure the plan must stop: %#v", next)
	}
}

func TestCleanupFailureStopsEverythingAndNeedsRepair(t *testing.T) {
	plan := BuildPlan(igcFacts())
	next := Advance(plan, ModeDPDKUIO, FailureCleanup)
	if !next.Stop || !next.NeedsRepair || next.NextMode != "" {
		t.Fatalf("cleanup failure must stop the plan and mark needs_repair: %#v", next)
	}
	for _, candidate := range plan.Candidates {
		next = Advance(plan, candidate.Mode, FailureCleanup)
		if !next.Stop || !next.NeedsRepair {
			t.Fatalf("cleanup failure on %s did not stop: %#v", candidate.Mode, next)
		}
	}
}

func TestBusyStopsAndNeedsRepairInsteadOfReplayingTheBadMode(t *testing.T) {
	plan := BuildPlan(igcFacts())
	next := Advance(plan, ModeDPDKUIO, FailureBusy)
	if !next.Stop || next.RetrySame || !next.NeedsRepair || next.NextMode != "" {
		t.Fatalf("busy decision = %#v, want stop+needs_repair and no replay", next)
	}
	if next := Advance(plan, ModeAFXDPCopy, FailureBusy); !next.Stop || next.RetrySame || !next.NeedsRepair {
		t.Fatalf("af_xdp busy decision = %#v, want stop+needs_repair", next)
	}
}
