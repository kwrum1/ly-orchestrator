package vpp

import (
	"strings"

	"ly-route/backend/internal/runtime/flow"
)

// IsQoSPluginGroup reports whether an object group is implemented by VPP's
// qos_plugin. ACL/policer-backed groups (vpp.acl.drop, vpp.behavior.rate,
// vpp.policer) use other plugins and remain supported when it is absent.
func IsQoSPluginGroup(kind string) bool {
	switch strings.TrimSpace(kind) {
	case "vpp.qos.classify", "vpp.qos.record", "vpp.qos.store", "vpp.qos.egress-map", "vpp.qos.mark":
		return true
	default:
		return false
	}
}

// WithoutQoSPluginGroups drops the object groups that require the QoS plugin.
func WithoutQoSPluginGroups(groups []flow.VPPObjectGroup) []flow.VPPObjectGroup {
	filtered := make([]flow.VPPObjectGroup, 0, len(groups))
	for _, group := range groups {
		if IsQoSPluginGroup(group.Kind) {
			continue
		}
		filtered = append(filtered, group)
	}
	return filtered
}
