package vpp

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CleanupDuplicateDataplaneTAPs releases TAP attachments that still carry the
// product name of a data interface the desired plan owns.
//
// VPP does not enforce interface name uniqueness on `set interface name`, and
// the installer-firstboot path used to pre-create a tap_bridge TAP with the
// product name while the runtime probe settled on AF_PACKET. The AF_PACKET
// rename then produced a second interface named lyroute-<port>, and every
// snapshot decode failed with
// `interface "lyroute-<port>" is ambiguous`.
//
// The cleanup runs before the apply snapshot so exactly one transport owns a
// port:
//   - desired transport is a TAP bridge: keep one TAP and delete duplicates
//     (the live one keeps forwarding; a replay does not churn it);
//   - desired transport is anything else: delete every TAP carrying the
//     product name, because the rename must claim it.
//
// A plan whose native path cannot be selected is left untouched: cleanup must
// never mutate VPP when the operator's apply is going to be rejected anyway.
func (a Adapter) CleanupDuplicateDataplaneTAPs(ctx context.Context, plan Plan) error {
	if a.Client == nil {
		return nil
	}
	targets, ok := desiredDataplaneTAPTargets(plan)
	if !ok || len(targets) == 0 {
		return nil
	}
	channel, err := a.Client.OpenChannel(ctx)
	if err != nil {
		return fmt.Errorf("open dataplane TAP cleanup channel: %w", err)
	}
	defer channel.Close()
	reply, err := channel.Do(ctx, Operation{
		Name:           "vpp.dataplane.tap-cleanup",
		RequestID:      plan.RequestID,
		Resource:       "dataplane",
		VPPCtlCommands: []string{"show tap"},
	})
	if err != nil {
		return fmt.Errorf("read VPP TAP inventory: %w", err)
	}
	payload, ok := reply.Payload.(VPPCTLReplyPayload)
	if !ok {
		return fmt.Errorf("read VPP TAP inventory returned payload %T", reply.Payload)
	}
	output := ""
	for _, result := range payload.CommandResults {
		if result.Command == "show tap" {
			if result.Retval != 0 {
				return fmt.Errorf("show tap returned VPP retval %d", result.Retval)
			}
			output = result.Stdout
			break
		}
	}
	if strings.TrimSpace(output) == "" {
		return nil
	}
	kept := map[string]bool{}
	deletes := make([]int, 0)
	for _, block := range tapInventoryBlocks(output) {
		index, names, ok := parseTAPInventoryBlock(block)
		if !ok {
			continue
		}
		for name := range names {
			hook, wanted := targets[name]
			if !wanted {
				continue
			}
			if hook == NativeHookTapBridge && !kept[name] {
				kept[name] = true
				break
			}
			deletes = append(deletes, index)
			break
		}
	}
	sort.Ints(deletes)
	for _, index := range deletes {
		if _, err := channel.Do(ctx, Operation{
			Name:           "vpp.dataplane.tap-cleanup.delete",
			RequestID:      plan.RequestID,
			Resource:       strconv.Itoa(index),
			VPPCtlCommands: []string{fmt.Sprintf("delete tap sw_if_index %d", index)},
		}); err != nil {
			return fmt.Errorf("remove duplicate dataplane TAP sw_if_index %d: %w", index, err)
		}
	}
	return nil
}

// desiredDataplaneTAPTargets maps every product VPP interface the plan attaches
// to the transport it will use. The second return value is false when the
// native path cannot be selected, which tells the caller to skip cleanup.
func desiredDataplaneTAPTargets(plan Plan) (map[string]NativeHook, bool) {
	targets := map[string]NativeHook{}
	if len(plan.NativePath.Assignments) == 0 {
		return targets, true
	}
	path, err := SelectNativePath(plan.NativePath)
	if err != nil {
		return nil, false
	}
	for _, attachment := range path.Attachments {
		name := strings.TrimSpace(attachment.VPPInterface)
		if name == "" {
			continue
		}
		targets[name] = attachment.Hook
	}
	return targets, true
}
