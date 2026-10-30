package service

import (
	"context"
	"strings"
	"time"
)

// LastApplyRecord is the receipt identity of the last successful apply of one
// service. It is used for unchanged components: an older transaction is the
// expected state, not a readback failure.
type LastApplyRecord struct {
	TransactionID string    `json:"transaction_id"`
	AppliedAt     time.Time `json:"applied_at"`
}

// LastApply returns the persisted identity of the last service apply. A
// missing record is reported as an error so callers can fall back to live
// health without inventing a transaction.
func (controller FilesystemController) LastApply(_ context.Context, service ServiceName) (LastApplyRecord, error) {
	record, err := controller.loadApplyRecord(service)
	if err != nil {
		return LastApplyRecord{}, err
	}
	return LastApplyRecord{
		TransactionID: strings.TrimSpace(record.TransactionID),
		AppliedAt:     record.AppliedAt,
	}, nil
}
