package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ly-route/backend/internal/runtime/applystate"
)

type receiptWritingRunner struct {
	receiptPath string
	environment []string
	runEnvCalls int
}

func (runner *receiptWritingRunner) Run(context.Context, string, ...string) error {
	return errors.New("helper was run without the caller environment")
}

func (runner *receiptWritingRunner) RunEnv(_ context.Context, environment []string, _ string, _ ...string) error {
	runner.environment = append([]string(nil), environment...)
	runner.runEnvCalls++
	receipt := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     "txn-caller",
		Status:            applystate.StatusApplied,
		DesiredGeneration: 3,
		AppliedGeneration: 3,
		DesiredHash:       "hash-three",
		Steps: []applystate.Step{{
			Command: "set interface state lan0 up", Status: "applied", Attempts: 1,
			Compensation: &applystate.Compensation{Status: "not-required"},
		}},
	}
	return applystate.WriteReceipt(runner.receiptPath, receipt)
}

func (runner *receiptWritingRunner) Output(context.Context, string, ...string) (string, error) {
	return "", nil
}

func (runner *receiptWritingRunner) Status(context.Context, ServiceName) (Health, error) {
	return Health{Available: true}, nil
}

func TestVPPApplyHelperReceivesCallerIdentityAndBindsReceipt(t *testing.T) {
	root := t.TempDir()
	receiptPath := filepath.Join(root, strings.TrimPrefix(vppApplyReceiptPath, "/"))
	runner := &receiptWritingRunner{receiptPath: receiptPath}
	controller := FilesystemController{RootDir: root, Runner: runner}
	ctx := withApplyIdentity(context.Background(), ApplyIdentity{
		TransactionID: "txn-caller", DesiredGeneration: 3, DesiredHash: "hash-three",
	})
	if err := controller.runApplyCommand(ctx, VPP, nil); err != nil {
		t.Fatalf("runApplyCommand: %v", err)
	}
	if runner.runEnvCalls != 1 {
		t.Fatalf("RunEnv calls = %d, want 1", runner.runEnvCalls)
	}
	joined := strings.Join(runner.environment, "\n")
	for _, required := range []string{
		"LY_ROUTE_APPLY_TRANSACTION_ID=txn-caller",
		"LY_ROUTE_DESIRED_GENERATION=3",
		"LY_ROUTE_APPLIED_GENERATION=3",
		"LY_ROUTE_DESIRED_HASH=hash-three",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("helper environment lacks %q: %v", required, runner.environment)
		}
	}
}

func TestValidateVPPApplyReceiptRequiresCallerTransactionAndGeneration(t *testing.T) {
	root := t.TempDir()
	receiptPath := filepath.Join(root, strings.TrimPrefix(vppApplyReceiptPath, "/"))
	if err := os.MkdirAll(filepath.Dir(receiptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	controller := FilesystemController{RootDir: root}
	ctx := withApplyIdentity(context.Background(), ApplyIdentity{
		TransactionID: "txn-caller", DesiredGeneration: 3, DesiredHash: "hash-three",
	})
	good := applystate.Receipt{
		SchemaVersion:     applystate.SchemaVersion,
		TransactionID:     "txn-caller",
		Status:            applystate.StatusApplied,
		DesiredGeneration: 3,
		AppliedGeneration: 3,
		DesiredHash:       "hash-three",
		Steps: []applystate.Step{{
			Command: "set interface state lan0 up", Status: "applied", Attempts: 1,
			Compensation: &applystate.Compensation{Status: "not-required"},
		}},
	}
	cases := []struct {
		name   string
		mutate func(*applystate.Receipt)
		want   string
	}{
		{
			name: "helper invented its own transaction",
			mutate: func(receipt *applystate.Receipt) {
				receipt.TransactionID = "vpp-apply-1793305314-6745"
				receipt.ExecutorTransactionID = receipt.TransactionID
			},
			want: "transaction_id",
		},
		{
			name: "helper did not echo generation",
			mutate: func(receipt *applystate.Receipt) {
				receipt.DesiredGeneration = 0
				receipt.AppliedGeneration = 0
			},
			want: "desired_generation receipt=0",
		},
		{
			name: "helper echoed a different generation",
			mutate: func(receipt *applystate.Receipt) {
				receipt.DesiredGeneration = 2
				receipt.AppliedGeneration = 2
			},
			want: "desired_generation receipt=2",
		},
		{
			name: "helper echoed a different hash",
			mutate: func(receipt *applystate.Receipt) {
				receipt.DesiredHash = "other-hash"
			},
			want: "desired_hash",
		},
		{
			name: "partial without reasons",
			mutate: func(receipt *applystate.Receipt) {
				receipt.Partial = true
				receipt.DegradedReasons = nil
			},
			want: "partial=true without degraded_reasons",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			receipt := good
			testCase.mutate(&receipt)
			if err := applystate.WriteReceipt(receiptPath, receipt); err != nil {
				t.Fatal(err)
			}
			err := controller.validateVPPApplyReceipt(ctx)
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("validate error = %v, want %q", err, testCase.want)
			}
		})
	}
	if err := applystate.WriteReceipt(receiptPath, good); err != nil {
		t.Fatal(err)
	}
	if err := controller.validateVPPApplyReceipt(ctx); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
}

func TestValidateVPPApplyReceiptMissingFileFails(t *testing.T) {
	root := t.TempDir()
	controller := FilesystemController{RootDir: root}
	ctx := withApplyIdentity(context.Background(), ApplyIdentity{
		TransactionID: "txn-caller", DesiredGeneration: 3, DesiredHash: "hash-three",
	})
	if err := controller.validateVPPApplyReceipt(ctx); !errors.Is(err, applystate.ErrReceiptMissing) {
		t.Fatalf("missing receipt error = %v, want ErrReceiptMissing", err)
	}
}

func TestMergeEnvironmentCallerWinsOverStaleEnvironmentFile(t *testing.T) {
	merged := mergeEnvironment(
		[]string{"PATH=/usr/bin", "LY_ROUTE_DESIRED_GENERATION=9", "LY_ROUTE_APPLY_TRANSACTION_ID=stale"},
		[]string{"LY_ROUTE_DESIRED_GENERATION=3", "LY_ROUTE_APPLY_TRANSACTION_ID=txn-caller"},
	)
	count := 0
	for _, entry := range merged {
		if strings.HasPrefix(entry, "LY_ROUTE_DESIRED_GENERATION=") {
			count++
			if entry != "LY_ROUTE_DESIRED_GENERATION=3" {
				t.Fatalf("stale generation won: %v", merged)
			}
		}
		if strings.HasPrefix(entry, "LY_ROUTE_APPLY_TRANSACTION_ID=") && entry != "LY_ROUTE_APPLY_TRANSACTION_ID=txn-caller" {
			t.Fatalf("stale transaction won: %v", merged)
		}
	}
	if count != 1 {
		t.Fatalf("duplicate generation env: %v", merged)
	}
}
