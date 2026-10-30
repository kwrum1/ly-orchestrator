package orchestrator

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestPolicyRepositoryPersistsTypedPolicyAndProtectsTopologyReferences(t *testing.T) {
	ctx := context.Background()
	repository := newTestRepository(t, ctx)
	topology := validPolicyTopology(t)
	if err := repository.Replace(ctx, topology); err != nil {
		t.Fatal(err)
	}
	policy, err := ParsePolicy(topology, validPolicyInput())
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.ReplacePolicy(ctx, policy); err != nil {
		t.Fatal(err)
	}
	stored, checksum, err := repository.PolicySnapshot(ctx)
	if err != nil || checksum == "" || !reflect.DeepEqual(stored.View(), policy.View()) {
		t.Fatalf("stored=%#v checksum=%q err=%v", stored.View(), checksum, err)
	}
	topologyChecksum, err := repository.Checksum(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteGroup(ctx, "inline-east"); !errors.Is(err, ErrDeletedPolicyReference) {
		t.Fatalf("delete referenced group error=%v", err)
	}
	after, err := repository.Checksum(ctx)
	if err != nil || after != topologyChecksum {
		t.Fatalf("topology changed after rejected delete: %q %v", after, err)
	}
	if err := repository.Delete(ctx); !errors.Is(err, ErrDeletedPolicyReference) {
		t.Fatalf("delete referenced topology error=%v", err)
	}
	if err := repository.DeletePolicy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repository.DeleteGroup(ctx, "inline-east"); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRepositoryPersistsCollectionOrderDeterministically(t *testing.T) {
	ctx := context.Background()
	repository := newTestRepository(t, ctx)
	topology := validPolicyTopology(t)
	if err := repository.Replace(ctx, topology); err != nil {
		t.Fatal(err)
	}

	forward := validPolicyInput()
	forward.Groups[0], forward.Groups[1] = forward.Groups[1], forward.Groups[0]
	forward.Groups[0].Position = 999
	forward.Groups[1].Position = 1
	forward.Groups[0].Rules[0].Sequence = 777
	forward.Groups[1].Rules[0].Sequence = 0
	if err := repository.ReplacePolicy(ctx, mustParsePolicy(t, forward)); err != nil {
		t.Fatal(err)
	}
	stored, forwardChecksum, err := repository.PolicySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.View().Groups[0].ID != "default" || stored.View().Groups[1].ID != "safety" {
		t.Fatalf("persisted group order = %#v, want default,safety", stored.View().Groups)
	}
	if stored.View().Groups[0].Position != 10 || stored.View().Groups[1].Position != 20 {
		t.Fatalf("persisted group ordinals = %d,%d, want 10,20", stored.View().Groups[0].Position, stored.View().Groups[1].Position)
	}

	reverse := clonePolicyInput(forward)
	reverse.Groups[0], reverse.Groups[1] = reverse.Groups[1], reverse.Groups[0]
	if err := repository.ReplacePolicy(ctx, mustParsePolicy(t, reverse)); err != nil {
		t.Fatal(err)
	}
	stored, reverseChecksum, err := repository.PolicySnapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stored.View().Groups[0].ID != "safety" || stored.View().Groups[1].ID != "default" {
		t.Fatalf("persisted group order after reverse = %#v, want safety,default", stored.View().Groups)
	}
	if forwardChecksum == reverseChecksum {
		t.Fatal("order-only policy change kept the persisted checksum; apply diff would miss it")
	}
}
