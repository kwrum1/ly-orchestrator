package httpapi

import (
	"fmt"
	"strings"

	"ly-route/backend/internal/runtime/applystate"
	"ly-route/backend/internal/runtime/vpp"
)

// gatewayExecutionRecordProvider is implemented by the production gateway
// transaction. It exposes the vppctl commands actually issued, so the Go path
// can publish the same auditable receipt as vpp-apply.
type gatewayExecutionRecordProvider interface {
	ResetExecutionRecords()
	ExecutionRecords() []vpp.CommandRecord
}

func (server *Server) resetGatewayExecutionRecords() {
	if provider, ok := server.gatewayTransaction.(gatewayExecutionRecordProvider); ok {
		provider.ResetExecutionRecords()
	}
}

func (server *Server) gatewayExecutionRecords() []vpp.CommandRecord {
	if provider, ok := server.gatewayTransaction.(gatewayExecutionRecordProvider); ok {
		return provider.ExecutionRecords()
	}
	return nil
}

func (server *Server) publishGoVPPReceipt(receipt applystate.Receipt) error {
	if strings.TrimSpace(server.vppReceiptPath) == "" {
		return nil
	}
	if err := applystate.WriteReceipt(server.vppReceiptPath, receipt); err != nil {
		return fmt.Errorf("write Go transaction receipt: %w", err)
	}
	return nil
}

// buildGoVPPReceipt turns the commands the Go transaction actually issued into
// the same receipt contract used by vpp-apply. Readbacks are kept visibly
// distinct from mutations; a failed command is never recorded as applied.
func buildGoVPPReceipt(transactionID, desiredHash string, generation int64, records []vpp.CommandRecord, status string, partial bool, reasons []string) applystate.Receipt {
	if strings.TrimSpace(status) == "" {
		status = applystate.StatusApplied
	}
	receipt := applystate.Receipt{
		SchemaVersion:         applystate.SchemaVersion,
		TransactionID:         transactionID,
		ExecutorTransactionID: "go-vpp-" + transactionID,
		Status:                status,
		DesiredGeneration:     generation,
		AppliedGeneration:     generation,
		DesiredHash:           desiredHash,
		Partial:               partial,
	}
	if status != applystate.StatusApplied {
		receipt.AppliedGeneration = 0
	}
	failed := false
	for _, record := range records {
		command := strings.TrimSpace(record.Command)
		if command == "" {
			continue
		}
		stepStatus := "applied"
		switch strings.TrimSpace(record.Status) {
		case "readback":
			stepStatus = "readback"
		case "rollback":
			stepStatus = "rollback"
		case "failed":
			stepStatus = "failed"
			receipt.Partial = true
			failed = true
			detail := firstLine(record.Error, 240)
			if detail == "" {
				detail = "command failed"
			}
			receipt.DegradedReasons = append(receipt.DegradedReasons, command+": "+detail)
		}
		compensation := &applystate.Compensation{Status: "not-required"}
		if stepStatus == "applied" {
			compensation = &applystate.Compensation{
				Status: "unsupported",
				Reason: "the typed control-plane transaction owns reverse-dependency rollback",
			}
		}
		attempts := record.Attempts
		if attempts < 1 {
			attempts = 1
		}
		receipt.Steps = append(receipt.Steps, applystate.Step{
			Command:      command,
			Status:       stepStatus,
			Attempts:     attempts,
			Compensation: compensation,
			Reason:       firstLine(record.Error, 240),
		})
	}
	if failed && receipt.Status == applystate.StatusApplied {
		// A transaction cannot claim applied while one of its own recorded
		// commands failed. The caller turns this into a failed apply instead of
		// committing a partial generation.
		receipt.Status = applystate.StatusFailed
		receipt.AppliedGeneration = 0
		receipt.Partial = true
	}
	if len(receipt.Steps) == 0 {
		stepStatus := "applied"
		if status != applystate.StatusApplied {
			stepStatus = "failed"
		}
		receipt.Steps = append(receipt.Steps, applystate.Step{
			Command:      "transaction:no-vpp-commands",
			Status:       stepStatus,
			Attempts:     1,
			Compensation: &applystate.Compensation{Status: "not-required"},
		})
	}
	for _, reason := range reasons {
		if reason = strings.TrimSpace(reason); reason != "" {
			receipt.DegradedReasons = append(receipt.DegradedReasons, reason)
		}
	}
	// The caller's partial hint is only meaningful together with a concrete
	// cause. Failed/needs-repair steps and named capability failures produce one
	// above; without them there is nothing to report and the transaction is not
	// partial. Never invent a generic "something degraded" placeholder.
	if len(receipt.DegradedReasons) > 0 {
		receipt.Partial = true
	} else {
		receipt.Partial = false
	}
	return receipt
}
