package apply

import (
	"context"
	"strings"
	"testing"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

func TestProductionGatewayTransactionRecordsExecutedVPPCommands(t *testing.T) {
	prior, desired, live := productionGatewayPlans()
	client := &desiredLiveClient{state: live}
	transaction := NewProductionGatewayTransaction(vpp.Adapter{Client: client}, deterministicClock(time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)))
	transaction.ResetExecutionRecords()
	if _, err := transaction.Run(context.Background(), productionReconciliationPlan("txn-execution-record", prior, desired)); err != nil {
		t.Fatal(err)
	}
	records := transaction.ExecutionRecords()
	if len(records) == 0 {
		t.Fatal("Go transaction recorded no commands")
	}
	hasReadback := false
	hasMutation := false
	for _, record := range records {
		if strings.TrimSpace(record.Command) == "" {
			t.Fatalf("empty command record: %#v", record)
		}
		if record.Attempts < 1 {
			t.Fatalf("attempts = %d: %#v", record.Attempts, record)
		}
		switch record.Status {
		case "readback":
			hasReadback = true
		case "applied":
			hasMutation = true
		}
	}
	if !hasReadback || !hasMutation {
		t.Fatalf("records lack readback/mutation distinction: %#v", records)
	}
	transaction.ResetExecutionRecords()
	if len(transaction.ExecutionRecords()) != 0 {
		t.Fatal("ResetExecutionRecords did not clear the log")
	}
}
