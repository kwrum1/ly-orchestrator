package service

import (
	"context"
	"testing"
	"time"
)

func TestFilesystemController_transactionReceiptCannotBeReboundByOlderStatusRead(t *testing.T) {
	now := time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)
	runner := &fakeRunner{
		health:  map[ServiceName]Health{Dnsmasq: {Service: Dnsmasq, Available: true}},
		outputs: map[string]string{"dnsmasq -t /etc/dnsmasq.d/ly-route-dhcp.conf": "configuration test passed"},
	}
	controller := FilesystemController{RootDir: t.TempDir(), Runner: runner, Now: func() time.Time { return now }}
	artifacts := []RenderedArtifact{NewArtifact(Dnsmasq, "/etc/dnsmasq.d/ly-route-dhcp.conf", "desired", "restart")}

	if err := controller.ReloadOrRestart(withTransactionID(context.Background(), "txn-new"), Dnsmasq, artifacts); err != nil {
		t.Fatal(err)
	}

	request := EvidenceRequest{TransactionID: "txn-new", Capability: "dnsmasq", Artifacts: artifacts}
	if _, err := controller.Receipt(context.Background(), request); err != nil {
		t.Fatalf("new transaction receipt failed: %v", err)
	}
	if _, err := controller.Readback(context.Background(), request); err != nil {
		t.Fatalf("new transaction readback failed: %v", err)
	}

	oldRequest := request
	oldRequest.TransactionID = "txn-old"
	if _, err := controller.Receipt(context.Background(), oldRequest); err == nil {
		t.Fatal("older status read rebound the current service receipt")
	}
	if _, err := controller.Readback(context.Background(), oldRequest); err == nil {
		t.Fatal("older status read accepted the current service receipt")
	}

}
