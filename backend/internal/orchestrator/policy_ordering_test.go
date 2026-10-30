package orchestrator

import (
	"encoding/json"
	"reflect"
	"testing"
)

func orderingFlow(t *testing.T) PolicyFlow {
	t.Helper()
	return mustParseFlow(t, FlowInput{SourceIP: "192.0.2.10", DestinationIP: "198.51.100.10", Protocol: ProtocolTCP, SourcePort: 50000, DestinationPort: 443})
}

func compiledPathJSON(t *testing.T, policy Policy, flow PolicyFlow) string {
	t.Helper()
	path := mustCompilePolicy(t, policy, flow, Prelude{})
	payload, err := json.Marshal(path)
	if err != nil {
		t.Fatalf("marshal compiled path: %v", err)
	}
	return string(payload)
}

func TestPolicy_collection_order_is_authoritative_and_numeric_fields_ignored(t *testing.T) {
	// Given
	flow := orderingFlow(t)
	base := validPolicyInput()
	baseView := mustParsePolicy(t, base).View()
	basePlan := compiledPathJSON(t, mustParsePolicy(t, base), flow)

	// Numeric fields are legacy noise: out-of-order, duplicate and zero values
	// must not validate, reorder, or otherwise change the persisted policy.
	numericNoise := clonePolicyInput(base)
	numericNoise.Groups[0].Position = 0
	numericNoise.Groups[1].Position = 0
	numericNoise.Groups[0].Rules[0].Sequence = 999
	numericNoise.Groups[1].Rules[0].Sequence = 999

	// When
	policy, err := ParsePolicy(validPolicyTopology(t), numericNoise)

	// Then
	if err != nil {
		t.Fatalf("ParsePolicy numeric noise: %v", err)
	}
	if view := policy.View(); !reflect.DeepEqual(view, baseView) {
		t.Fatalf("numeric fields changed the persisted view: %#v != %#v", view, baseView)
	}
	if plan := compiledPathJSON(t, policy, flow); plan != basePlan {
		t.Fatalf("numeric fields changed the compiled plan:\n%s\n%s", plan, basePlan)
	}
	if policy.groups[0].position != 10 || policy.groups[1].position != 20 {
		t.Fatalf("persisted group ordinals = %d, %d; want 10, 20", policy.groups[0].position, policy.groups[1].position)
	}
	if policy.groups[0].rules[0].sequence != 10 || policy.groups[1].rules[0].sequence != 10 {
		t.Fatalf("persisted rule ordinals = %d, %d; want 10, 10", policy.groups[0].rules[0].sequence, policy.groups[1].rules[0].sequence)
	}
}

func TestPolicy_group_collection_order_controls_group_precedence(t *testing.T) {
	// Given
	flow := orderingFlow(t)
	input := validPolicyInput()
	before := mustCompilePolicy(t, mustParsePolicy(t, input), flow, Prelude{})
	input.Groups[0], input.Groups[1] = input.Groups[1], input.Groups[0]

	// When
	after := mustCompilePolicy(t, mustParsePolicy(t, input), flow, Prelude{})

	// Then
	if before.Decisions[0].PolicyGroup != "safety" || after.Decisions[0].PolicyGroup != "default" {
		t.Fatalf("group precedence before/after = %#v / %#v", before.Decisions, after.Decisions)
	}
	if before.Traversal[0] != "inline-east" || after.Traversal[0] != "inline-west" {
		t.Fatalf("group reorder did not change traversal: before=%#v after=%#v", before.Traversal, after.Traversal)
	}
	if before.Decisions[1].PolicyGroup != "default" || after.Decisions[1].PolicyGroup != "safety" {
		t.Fatalf("group reorder did not reverse the remaining decision: before=%#v after=%#v", before.Decisions, after.Decisions)
	}
}

func TestPolicy_rule_collection_order_flips_allow_and_block(t *testing.T) {
	// Given: two rules match the same target; the first one wins.
	allow := PolicyRuleInput{ID: "allow", Match: anyMatch(), Action: ActionInput{Kind: ActionDirect}}
	block := PolicyRuleInput{ID: "block", Match: anyMatch(), Action: ActionInput{Kind: ActionDrop}}
	input := validPolicyInput()
	input.Groups = []PolicyGroupInput{{ID: "conflict", Rules: []PolicyRuleInput{allow, block}}}
	flow := orderingFlow(t)

	// When
	allowed := mustCompilePolicy(t, mustParsePolicy(t, input), flow, Prelude{})
	input.Groups[0].Rules[0], input.Groups[0].Rules[1] = input.Groups[0].Rules[1], input.Groups[0].Rules[0]
	blocked := mustCompilePolicy(t, mustParsePolicy(t, input), flow, Prelude{})

	// Then
	if allowed.Exit != PathExitLAN || allowed.Decisions[0].RuleID != "allow" || allowed.Decisions[0].Action != ActionDirect {
		t.Fatalf("allow-first path = %#v", allowed)
	}
	if blocked.Exit != PathExitDrop || blocked.Decisions[0].RuleID != "block" || blocked.Decisions[0].Action != ActionDrop {
		t.Fatalf("block-first path = %#v", blocked)
	}
	if allowed.Exit == blocked.Exit {
		t.Fatal("rule order did not flip allow vs block")
	}
}

func TestPolicy_order_only_changes_compiled_plan_and_persisted_document(t *testing.T) {
	// Given
	flow := orderingFlow(t)
	input := validPolicyInput()
	base := mustParsePolicy(t, input)
	baseView, err := json.Marshal(base.View())
	if err != nil {
		t.Fatalf("marshal base view: %v", err)
	}
	basePlan := compiledPathJSON(t, base, flow)

	// When: only the rule array order changes (IDs, matches and actions stay).
	input.Groups[0].Rules = append([]PolicyRuleInput{
		{ID: "safety-direct", Match: anyMatch(), Action: ActionInput{Kind: ActionDirect}},
	}, input.Groups[0].Rules...)

	// Then
	reordered := mustParsePolicy(t, input)
	reorderedView, err := json.Marshal(reordered.View())
	if err != nil {
		t.Fatalf("marshal reordered view: %v", err)
	}
	if string(reorderedView) == string(baseView) {
		t.Fatal("order-only edit did not change the persisted policy document")
	}
	if plan := compiledPathJSON(t, reordered, flow); plan == basePlan {
		t.Fatal("order-only edit did not change the compiled plan")
	}
}

func TestPolicy_compilation_is_deterministic_for_the_same_order(t *testing.T) {
	// Given
	flow := orderingFlow(t)
	input := validPolicyInput()
	input.Groups[0], input.Groups[1] = input.Groups[1], input.Groups[0]
	want := compiledPathJSON(t, mustParsePolicy(t, input), flow)
	wantView, err := json.Marshal(mustParsePolicy(t, input).View())
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}

	// When / Then: repeated parses of the same ordered input are bit-identical.
	for attempt := 0; attempt < 20; attempt++ {
		policy, parseErr := ParsePolicy(validPolicyTopology(t), clonePolicyInput(input))
		if parseErr != nil {
			t.Fatalf("ParsePolicy attempt %d: %v", attempt, parseErr)
		}
		if got := compiledPathJSON(t, policy, flow); got != want {
			t.Fatalf("attempt %d compiled a different plan:\n%s\n%s", attempt, got, want)
		}
		view, marshalErr := json.Marshal(policy.View())
		if marshalErr != nil {
			t.Fatalf("marshal view attempt %d: %v", attempt, marshalErr)
		}
		if string(view) != string(wantView) {
			t.Fatalf("attempt %d persisted a different document:\n%s\n%s", attempt, view, wantView)
		}
	}
}
