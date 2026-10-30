package httpapi

import (
	"context"
	"fmt"
	"log"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/vpp"
)

// failedApplyObjectPattern extracts the resource the gateway transaction was
// working on. The graph wraps adapter failures as "resource <name>: ...".
var failedApplyObjectPattern = regexp.MustCompile(`resource (interfaces|bonds|wan-groups|service-networks|routes|acls|qos|nat44|port-maps):`)

// failedApplyObjectKeywords maps a plan/validation error to the same object
// vocabulary when the error never reached a resource node (for example a DHCP
// service document rejected while the runtime plan was compiled).
var failedApplyObjectKeywords = []struct {
	object   string
	keywords []string
}{
	{"dhcp_server", []string{"dhcp service", "dhcp server", "dnsmasq"}},
	{"routes", []string{"route policy", "route-policy", "pre-nat", "next_hop", "next-hop", "wan group", "wan-group"}},
	{"interfaces", []string{"interface", "lcp", "lan address", "address assignment"}},
	{"service-networks", []string{"service network", "dns service", "tap"}},
	{"nat44", []string{"nat44", "port map", "port-map", "static mapping"}},
	{"acls", []string{"acl", "security"}},
}

// publishFailedRuntimeResult records a failed transaction without replacing the
// committed generation. `last_apply` must keep pointing at the previous
// committed snapshot; the failure is exposed separately as `last_failed_apply`.
func (server *Server) publishFailedRuntimeResult(lease *applyLease, result RuntimeApplyResult) {
	if lease != nil && lease.expired.Load() {
		return
	}
	server.runtimeMu.Lock()
	server.lastFailedApply = &result
	// A newer failed apply is the authoritative explanation for why the
	// desired generation is not live; do not leave the older startup
	// reconcile notice as the visible reason.
	server.lastReconcile = nil
	server.runtimeMu.Unlock()
	// A superseded apply is not a dataplane failure: the plan was stale, nothing
	// committed, and the newest desired generation still has to reach VPP.
	// Replaying the last committed generation here would strand the operator's
	// change (and did: a WAN role edit left a uio-bound port without ownership
	// records). Converge on the newest generation instead.
	if supersededApply.Swap(false) {
		server.scheduleConvergeApply()
		return
	}
	// A failed transaction can leave the dataplane half applied (the desktop
	// run showed a rolled back role change that dropped the LAN address and
	// left VPP with raw interfaces). The recovery unit replays the last
	// committed generation, so request it instead of leaving the device in a
	// state that only a manual power cycle clears.
	server.requestRecoveryAfterFailure(result)
}

func (server *Server) lastFailedRuntimeApply() *RuntimeApplyResult {
	server.runtimeMu.Lock()
	defer server.runtimeMu.Unlock()
	if server.lastFailedApply == nil {
		return nil
	}
	failed := *server.lastFailedApply
	return &failed
}

// committedApplySummary describes the generation the appliance is still
// running. It is included in a failed apply response so the operator can see
// that the previous generation stayed authoritative.
func (server *Server) committedApplySummary() map[string]any {
	result := server.runtimeEvidence()
	if result.Status != RuntimeStatusCommitted {
		return nil
	}
	return map[string]any{
		"status":         result.Status,
		"runtime_state":  result.RuntimeState,
		"transaction_id": result.TransactionID,
		"snapshot_hash":  result.SnapshotHash,
		"applied_at":     result.AppliedAt,
	}
}

// applyErrorEnvelope turns the internal error chain into an actionable object.
// The operator gets the failing object, the step, and a concrete suggestion;
// the full chain stays in `details`.
func applyErrorEnvelope(cause string, receipt apply.RollbackReceipt, repaired []string) map[string]any {
	object := failedApplyObject(cause)
	step := "apply"
	rolledBack := receipt.Status == apply.ReceiptRolledBack
	if receipt.Status == apply.ReceiptFailed {
		step = "rollback"
	}
	suggestion := "修正或删除被拒绝的配置文档，然后重新 apply；网关仍运行上一代已提交配置。"
	switch object {
	case "routes":
		suggestion = "检查路由策略的 next_hop 是否为入口 LAN 可达的 IPv4 地址，并确认每条策略的源地址都落在同一个 LAN（VPP 的 pre-NAT 分类器每个设备只支持一个入口/一个 LAN 前缀）；修正或删除该策略后重新 apply。"
	case "interfaces", "bonds":
		suggestion = "检查 LAN/WAN 端口角色与地址是否冲突；回滚会保留上一代地址，修复端口配置后重新 apply。"
	case "dhcp_server", "service-networks":
		suggestion = "确认 DHCP 服务绑定的端口是已配置地址的 LAN 口（WAN 口不能提供 DHCP）；修正接口后重新 apply。"
	case "nat44", "port-maps":
		suggestion = "检查 NAT 内部主机/端口与 WAN 线路是否匹配；修正对象后重新 apply。"
	}
	envelope := map[string]any{
		"code":        "apply_failed",
		"object":      object,
		"step":        step,
		"message":     firstLine(cause, 320),
		"suggestion":  suggestion,
		"rolled_back": rolledBack,
		"details":     cause,
	}
	if len(repaired) > 0 {
		envelope["repaired"] = repaired
	}
	if receipt.RollbackError != "" {
		envelope["rollback_error"] = firstLine(receipt.RollbackError, 320)
	}
	return envelope
}

// failedApplyObject names the configuration object an error is about. The graph
// prefix is authoritative; the keyword fallback covers failures raised while the
// runtime plan was compiled, which never reach a resource node.
func failedApplyObject(cause string) string {
	if match := failedApplyObjectPattern.FindStringSubmatch(cause); len(match) == 2 {
		return match[1]
	}
	lowered := strings.ToLower(cause)
	for _, candidate := range failedApplyObjectKeywords {
		for _, keyword := range candidate.keywords {
			if strings.Contains(lowered, keyword) {
				return candidate.object
			}
		}
	}
	return "gateway"
}

// applyFailureResponse is the operator-facing body of a rejected or rolled-back
// apply. It names the object, the step and a suggestion, and reports the
// generation that is still committed. A failed apply is never a 5xx: the
// request was understood and the appliance deliberately stayed on (or returned
// to) the previous generation.
func (server *Server) applyFailureResponse(result RuntimeApplyResult, cause string, repaired []string) map[string]any {
	rolledBack := result.RollbackReceipt.Status == apply.ReceiptRolledBack
	reason := "apply failed; the previous committed generation is still in force: "
	if rolledBack {
		reason = "apply failed; the previous committed generation was restored: "
	} else if result.RollbackReceipt.Status == apply.ReceiptFailed {
		reason = "apply failed and rollback reported errors; the previous committed generation remains in force and the LAN was re-verified: "
	}
	response := map[string]any{
		"status":           result.Status,
		"runtime_state":    result.RuntimeState,
		"reason":           reason + firstLine(cause, 400),
		"transaction_id":   result.TransactionID,
		"rollback_receipt": result.RollbackReceipt,
		"components":       result.Components,
		"rolled_back":      rolledBack,
		"committed":        server.committedApplySummary(),
		"error":            applyErrorEnvelope(cause, result.RollbackReceipt, repaired),
	}
	return response
}

func firstLine(value string, limit int) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexAny(value, "\r\n"); index >= 0 {
		value = value[:index]
	}
	if limit > 0 && len(value) > limit {
		value = value[:limit] + "..."
	}
	return value
}

// repairLANAddresses is the last-resort safety net after a failed rollback. It
// only ever adds addresses, so it cannot take down an interface that still
// forwards. VPP is repaired first (the Linux LCP mirrors it), then the host
// interface is checked and repaired directly.
func (server *Server) repairLANAddresses(ctx context.Context, plan apply.Plan) []string {
	assignments := rollbackLANAssignments(plan)
	if len(assignments) == 0 {
		return nil
	}
	vppctl := envOrDefault("LY_ROUTE_VPPCTL", "vppctl")
	repaired := make([]string, 0)
	for _, assignment := range assignments {
		cidr := strings.TrimSpace(assignment.CIDR)
		vppInterface := strings.TrimSpace(assignment.VPPInterface)
		linuxInterface := strings.TrimSpace(assignment.LinuxInterface)
		if cidr == "" || vppInterface == "" || linuxInterface == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(cidr); err != nil || !prefix.Addr().Is4() {
			continue
		}
		hostInterface := vpp.LANControlPlaneHostInterface(linuxInterface)
		// Replay the VPP address (optional on VPP: an existing address is fine).
		_, _ = exec.CommandContext(ctx, vppctl, "set", "interface", "ip", "address", vppInterface, cidr).CombinedOutput()
		_, _ = exec.CommandContext(ctx, vppctl, "set", "interface", "state", vppInterface, "up").CombinedOutput()
		observed, _ := exec.CommandContext(ctx, "ip", "-br", "addr", "show", hostInterface).CombinedOutput()
		if strings.Contains(string(observed), cidr) {
			continue
		}
		output, err := exec.CommandContext(ctx, "ip", "addr", "add", cidr, "dev", hostInterface).CombinedOutput()
		if err != nil {
			log.Printf("apply rollback safety net: ip addr add %s dev %s: %v: %s", cidr, hostInterface, err, strings.TrimSpace(string(output)))
			continue
		}
		_, _ = exec.CommandContext(ctx, "ip", "link", "set", "dev", hostInterface, "up").CombinedOutput()
		repaired = append(repaired, fmt.Sprintf("%s %s", hostInterface, cidr))
		log.Printf("apply rollback safety net: restored %s on %s", cidr, hostInterface)
	}
	return repaired
}

// rollbackLANAssignments prefers the previous committed generation: a LAN the
// failed apply tried to remove must not be resurrected by the safety net.
func rollbackLANAssignments(plan apply.Plan) []vpp.AddressAssignment {
	source := plan.GatewayPlan.AddressAssignments
	if plan.Previous.GatewayPlan != nil && len(plan.Previous.GatewayPlan.AddressAssignments) > 0 {
		source = plan.Previous.GatewayPlan.AddressAssignments
	}
	assignments := make([]vpp.AddressAssignment, 0, len(source))
	for _, assignment := range source {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "lan") {
			continue
		}
		assignments = append(assignments, assignment)
	}
	return assignments
}

// dhcpTargetIsExplicitWAN reports whether the DHCP server's port is explicitly
// configured as a WAN link. Unknown/unassigned roles stay writable so a role
// transition is still possible; an explicit WAN binding is rejected because no
// dataplane can serve DHCP from it.
func (server *Server) dhcpTargetIsExplicitWAN(ctx context.Context, reference string) bool {
	canonical := server.canonicalInterfaceReference(ctx, reference)
	if canonical == "" {
		return false
	}
	linux := server.resolveInterfaceID(ctx, canonical)
	assignments, err := server.runtimeAddressAssignments(ctx)
	if err != nil {
		return false
	}
	return explicitWANAssignment(assignments, canonical, linux)
}

// explicitWANAssignment matches an assignment list against a port reference.
// It is shared by the document write path and the runtime plan so both enforce
// the same rule.
func explicitWANAssignment(assignments []vpp.AddressAssignment, canonical, linux string) bool {
	for _, assignment := range assignments {
		if !strings.EqualFold(strings.TrimSpace(assignment.Role), "wan") {
			continue
		}
		if linux != "" && strings.EqualFold(strings.TrimSpace(assignment.LinuxInterface), linux) {
			return true
		}
		if canonical != "" && (strings.EqualFold(strings.TrimSpace(assignment.VPPInterface), canonical) ||
			strings.EqualFold(strings.TrimSpace(assignment.ID), canonical)) {
			return true
		}
	}
	return false
}
