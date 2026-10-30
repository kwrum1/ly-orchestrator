package vpp

import (
	"context"
	"testing"
)

type recorderTestClient struct{}

func (recorderTestClient) OpenChannel(context.Context) (Channel, error) {
	return recorderTestChannel{}, nil
}

type recorderTestChannel struct{}

func (recorderTestChannel) Do(context.Context, Operation) (Reply, error) {
	return Reply{Payload: VPPCTLReplyPayload{CommandResults: []VPPCTLCommandResult{{
		Command: "set interface state lyroute-lan0 up",
		Stdout:  "ok",
	}}}}, nil
}

func (recorderTestChannel) Close() error { return nil }

func TestRecordingClientCapturesCommands(t *testing.T) {
	recorder := NewExecutionRecorder()
	client := NewRecordingClient(recorderTestClient{}, recorder)
	channel, err := client.OpenChannel(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Do(context.Background(), Operation{
		Name: "vpp.dataplane.attach", RequestID: "txn-recorder",
		VPPCtlCommands: []string{"set interface state lyroute-lan0 up"},
	}); err != nil {
		t.Fatal(err)
	}
	records := recorder.Records()
	if len(records) != 1 || records[0].Command != "set interface state lyroute-lan0 up" || records[0].Status != "applied" {
		t.Fatalf("records = %#v", records)
	}
}
