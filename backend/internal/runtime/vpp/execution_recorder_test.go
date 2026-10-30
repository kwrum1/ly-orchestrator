package vpp

import "testing"

func TestExecutionRecorderRecordsCommandsReadbacksAndFailures(t *testing.T) {
	recorder := NewExecutionRecorder()
	recorder.record(Operation{Name: "vpp.route-policy", Resource: "route-1", VPPCtlCommands: []string{"set ly-route pre-nat-route add id 1"}},
		Reply{Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{{
			Command: "set ly-route pre-nat-route add id 1",
			Stdout:  "ok",
		}}}}, nil)
	recorder.record(Operation{Name: "vpp.route-policy.snapshot", VPPCtlCommands: []string{"show ly-route pre-nat-route"}},
		Reply{Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{{
			Command: "show ly-route pre-nat-route",
			Stdout:  "id 1",
		}}}}, nil)
	recorder.record(Operation{Name: "vpp.nat44.port-map", VPPCtlCommands: []string{"set nat44 ..."}},
		Reply{Retval: 1, Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{{
			Command: "set nat44 ...",
			Stderr:  "cli line error",
			Retval:  1,
		}}}}, nil)
	records := recorder.Records()
	if len(records) != 3 {
		t.Fatalf("records = %#v", records)
	}
	if records[0].Status != "applied" || records[0].Command != "set ly-route pre-nat-route add id 1" {
		t.Fatalf("mutation record = %#v", records[0])
	}
	if records[1].Status != "readback" {
		t.Fatalf("readback record = %#v", records[1])
	}
	if records[2].Status != "failed" || records[2].Stderr == "" {
		t.Fatalf("failed record = %#v", records[2])
	}
	recorder.Reset()
	if len(recorder.Records()) != 0 {
		t.Fatal("reset did not clear records")
	}
}
