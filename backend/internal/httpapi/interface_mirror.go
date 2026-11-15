package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"ly-route/backend/internal/runtime/vpp"
)

// Port mirroring ("镜像口") is a gateway-product capability layered on the
// interface resource:
//
//	{ "id": "eth2", "mirror": true,
//	  "mirror_sources": [{"interface": "eth1", "direction": "both"}] }
//
// The marked document is the mirror destination. Every source entry is another
// physical data port mirrored into it. VPP owns the session; the desired
// generation is compiled into the durable vpp operations file so the same
// generation is replayed after a VPP restart. Sources default to L3 (no l2
// keyword): routed gateway ports only traverse the device-input/interface
// output arcs, where span-l2-input/output is not registered.

type interfaceMirrorSource struct {
	Interface string
	Direction string
	L2        bool
}

func interfaceMirrorEnabled(item map[string]any) bool {
	return truthy(item["mirror"])
}

// interfaceMirrorSourcesOf accepts both the documented object form and a plain
// list of port ids (direction both, l2 on).
func interfaceMirrorSourcesOf(item map[string]any) []interfaceMirrorSource {
	raw, present := item["mirror_sources"]
	if !present || raw == nil {
		return nil
	}
	sources := []interfaceMirrorSource{}
	appendSource := func(name, direction string, l2 *bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		normalized, err := vpp.NormalizeMirrorDirection(direction)
		if err != nil {
			// Kept as typed so write-time validation can name the bad value.
			normalized = strings.ToLower(strings.TrimSpace(direction))
		}
		enabled := false
		if l2 != nil {
			enabled = *l2
		}
		sources = append(sources, interfaceMirrorSource{Interface: name, Direction: normalized, L2: enabled})
	}
	switch value := raw.(type) {
	case []any:
		for _, entry := range value {
			switch typed := entry.(type) {
			case string:
				appendSource(typed, "", nil)
			case map[string]any:
				var l2 *bool
				if flag, ok := typed["l2"].(bool); ok {
					l2 = &flag
				}
				appendSource(stringField(typed, "interface"), stringField(typed, "direction"), l2)
			}
		}
	case []string:
		for _, entry := range value {
			appendSource(entry, "", nil)
		}
	case []map[string]any:
		for _, typed := range value {
			var l2 *bool
			if flag, ok := typed["l2"].(bool); ok {
				l2 = &flag
			}
			appendSource(stringField(typed, "interface"), stringField(typed, "direction"), l2)
		}
	}
	return sources
}

// appendMirrorDataInterfaces adds the mirror destination/sources to the native
// dataplane attachment list. A mirror port usually has no LAN/WAN role and
// therefore no address assignment, so without this its VPP interface would not
// be part of the generation and the SPAN command could not be compiled.
func appendMirrorDataInterfaces(mirror *vpp.MirrorConfig, names []string) []string {
	if mirror == nil {
		return names
	}
	seen := make(map[string]struct{}, len(names)+len(mirror.Sources)+1)
	result := make([]string, 0, len(names)+len(mirror.Sources)+1)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	appendName := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, duplicate := seen[name]; duplicate {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	appendName(mirror.LinuxInterface)
	for _, source := range mirror.Sources {
		appendName(source.LinuxInterface)
	}
	return result
}

// desiredInterfaceMirrorFields returns the mirror attributes of every stored
// interface document, keyed by Linux netdev. The live interface snapshot is
// built from telemetry (which knows nothing about desired port attributes), so
// the read path overlays these fields; without it the 网卡设置 page would save
// the mirror port and then render it as unconfigured.
func (server *Server) desiredInterfaceMirrorFields(ctx context.Context) map[string]map[string]any {
	result := map[string]map[string]any{}
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return result
	}
	for _, item := range items {
		if truthy(item["deleted"]) {
			continue
		}
		_, hasMirror := item["mirror"]
		_, hasSources := item["mirror_sources"]
		if !hasMirror && !hasSources {
			continue
		}
		id := nonEmpty(stringField(item, "interface_id"), stringField(item, "id"))
		linux := strings.TrimSpace(server.resolveInterfaceID(ctx, id))
		if linux == "" {
			continue
		}
		result[linux] = map[string]any{
			"mirror":         truthy(item["mirror"]),
			"mirror_sources": item["mirror_sources"],
		}
	}
	return result
}

// runtimeMirrorConfig compiles the desired mirror generation. It is also the
// plan-time validation gate: a persisted document that an older build accepted
// still cannot produce a broken VPP command list.
func (server *Server) runtimeMirrorConfig(ctx context.Context) (*vpp.MirrorConfig, error) {
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return nil, err
	}
	var target map[string]any
	for _, item := range items {
		if truthy(item["deleted"]) || !interfaceMirrorEnabled(item) {
			continue
		}
		if target != nil {
			return nil, fmt.Errorf("only one interface can be the mirror port, found %s and %s",
				firstNonEmpty(stringField(target, "id"), stringField(target, "interface_id")),
				firstNonEmpty(stringField(item, "id"), stringField(item, "interface_id")))
		}
		target = item
	}
	if target == nil {
		return nil, nil
	}
	destinationID := firstNonEmpty(stringField(target, "interface_id"), stringField(target, "id"))
	destinationLinux := strings.TrimSpace(server.resolveInterfaceID(ctx, destinationID))
	if destinationLinux == "" {
		return nil, fmt.Errorf("mirror port %q is not a port on this device", destinationID)
	}
	if role, _ := interfaceDesiredRole(target); role != "" {
		return nil, fmt.Errorf("mirror port %q must not have a %s role", destinationID, strings.ToUpper(role))
	}
	if role, ok := boundaryRoleOf(server.orchestratorBoundaryPorts(ctx), []string{destinationID, destinationLinux}); ok {
		return nil, fmt.Errorf("mirror port %q is the %s boundary port; move the boundary first", destinationID, strings.ToUpper(role))
	}
	if stored := server.interfaceStoredItem(ctx, destinationID, destinationLinux); stored != nil {
		if bond := strings.TrimSpace(nonEmpty(stringField(stored, "bond"), nonEmpty(stringField(stored, "bond_id"), stringField(stored, "member_of")))); bond != "" {
			return nil, fmt.Errorf("mirror port %q belongs to a bond; remove it from the bond first", destinationID)
		}
	}
	mirror := &vpp.MirrorConfig{Destination: vppInterfaceName(destinationLinux), LinuxInterface: destinationLinux}
	for _, source := range interfaceMirrorSourcesOf(target) {
		sourceLinux := strings.TrimSpace(server.resolveInterfaceID(ctx, source.Interface))
		if sourceLinux == "" {
			return nil, fmt.Errorf("mirror source %q is not a port on this device", source.Interface)
		}
		if strings.EqualFold(sourceLinux, destinationLinux) {
			return nil, fmt.Errorf("mirror port %q cannot also be a mirror source", destinationID)
		}
		direction, err := vpp.NormalizeMirrorDirection(source.Direction)
		if err != nil {
			return nil, err
		}
		mirror.Sources = append(mirror.Sources, vpp.MirrorSource{
			Interface:      vppInterfaceName(sourceLinux),
			LinuxInterface: sourceLinux,
			Direction:      direction,
			L2:             source.L2,
		})
	}
	return mirror, nil
}

// runtimeMirrorPorts lists the product ids that take part in the desired mirror
// generation. The interface payload normalizer uses it to store canonical ids.
func (server *Server) runtimeMirrorPorts(ctx context.Context) map[string]string {
	ports := map[string]string{}
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return ports
	}
	for _, item := range items {
		if truthy(item["deleted"]) || !interfaceMirrorEnabled(item) {
			continue
		}
		if id := strings.TrimSpace(nonEmpty(stringField(item, "interface_id"), stringField(item, "id"))); id != "" {
			ports[id] = strings.TrimSpace(server.resolveInterfaceID(ctx, id))
		}
	}
	return ports
}

// validateInterfaceMirror enforces the write-time rules for the mirror port
// model. It also normalizes mirror_sources to the documented object form.
func (server *Server) validateInterfaceMirror(ctx context.Context, payload map[string]any) error {
	rawMirror, mirrorProvided := payload["mirror"]
	if !truthy(rawMirror) {
		// Clearing the flag must also clear the sources: leaving them behind
		// makes a later re-mark bind sessions the operator never selected.
		if mirrorProvided {
			payload["mirror"] = false
			payload["mirror_sources"] = []any{}
		}
		return nil
	}
	id := strings.TrimSpace(nonEmpty(stringField(payload, "interface_id"), stringField(payload, "id")))
	linuxName := strings.TrimSpace(server.resolveInterfaceID(ctx, id))
	if linuxName == "" {
		return fmt.Errorf("mirror port %q is not a port on this device", id)
	}
	if role, _ := interfaceDesiredRole(payload); role != "" {
		return fmt.Errorf("interface %s is the mirror port; unmark the mirror port before assigning it the %s role", id, strings.ToUpper(role))
	}
	management := strings.TrimSpace(server.resolveInterfaceID(ctx, server.managementInterfaceID(ctx)))
	if management != "" && strings.EqualFold(linuxName, management) {
		return fmt.Errorf("the management interface cannot be the mirror port")
	}
	if !server.takeoverFactsForPort(ctx, linuxName).Physical {
		return fmt.Errorf("mirror port %q is not a physical data port", id)
	}
	// The boundary role lives in the topology document, not in the interface
	// document being patched, so a payload-only role check missed it: marking
	// the LAN/WAN boundary port as the mirror destination returned 200 and
	// created a second identity for the same physical port.
	if role, ok := boundaryRoleOf(server.orchestratorBoundaryPorts(ctx), []string{id, linuxName}); ok {
		return fmt.Errorf("interface %s is the %s boundary port; move the boundary before marking it as the mirror port", id, strings.ToUpper(role))
	}
	if stored := server.interfaceStoredItem(ctx, id, linuxName); stored != nil {
		if role, _ := interfaceDesiredRole(stored); role != "" {
			return fmt.Errorf("interface %s already has the %s role; clear it before marking it as the mirror port", id, strings.ToUpper(role))
		}
		if bond := strings.TrimSpace(nonEmpty(stringField(stored, "bond"), nonEmpty(stringField(stored, "bond_id"), stringField(stored, "member_of")))); bond != "" {
			return fmt.Errorf("interface %s belongs to a bond; remove it from the bond before marking it as the mirror port", id)
		}
	}
	if groups := server.orchestratorGroupPorts(ctx); len(groups) > 0 {
		if groups[linuxName] {
			return fmt.Errorf("mirror port %q belongs to an orchestration group; remove it from the group first", id)
		}
	}
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return err
	}
	for _, item := range items {
		if truthy(item["deleted"]) || !interfaceMirrorEnabled(item) {
			continue
		}
		otherID := strings.TrimSpace(nonEmpty(stringField(item, "interface_id"), stringField(item, "id")))
		otherLinux := strings.TrimSpace(server.resolveInterfaceID(ctx, otherID))
		if strings.EqualFold(otherLinux, linuxName) {
			continue
		}
		return fmt.Errorf("interface %s is already the mirror port; unmark it before marking %s", otherID, id)
	}
	sources := interfaceMirrorSourcesOf(payload)
	normalized := make([]any, 0, len(sources))
	seen := map[string]struct{}{}
	for _, source := range sources {
		sourceLinux := strings.TrimSpace(server.resolveInterfaceID(ctx, source.Interface))
		if sourceLinux == "" {
			return fmt.Errorf("mirror source %q is not a port on this device", source.Interface)
		}
		if strings.EqualFold(sourceLinux, linuxName) {
			return fmt.Errorf("the mirror port cannot also be one of its own sources")
		}
		if management != "" && strings.EqualFold(sourceLinux, management) {
			return fmt.Errorf("the management interface cannot be a mirror source")
		}
		if !server.takeoverFactsForPort(ctx, sourceLinux).Physical {
			return fmt.Errorf("mirror source %q is not a physical data port", source.Interface)
		}
		if _, duplicate := seen[sourceLinux]; duplicate {
			continue
		}
		seen[sourceLinux] = struct{}{}
		direction, err := vpp.NormalizeMirrorDirection(source.Direction)
		if err != nil {
			return fmt.Errorf("mirror source %s: %v", source.Interface, err)
		}
		normalized = append(normalized, map[string]any{
			"interface": server.canonicalInterfaceID(ctx, sourceLinux),
			"direction": direction,
			"l2":        source.L2,
		})
	}
	payload["mirror"] = true
	payload["mirror_sources"] = normalized
	return nil
}

// orchestratorGroupPorts reports the data ports referenced by the persisted
// orchestrator topology. Gateway appliances keep this empty; the check exists
// so a shared interface document can never become both an orchestration group
// member and the gateway mirror port.
func (server *Server) orchestratorGroupPorts(ctx context.Context) map[string]bool {
	ports := map[string]bool{}
	if server.store == nil {
		return ports
	}
	documents, err := server.store.Configs(ctx, orchestratorTopologyConfigResource)
	if err != nil {
		return ports
	}
	for _, document := range documents {
		var pack orchestratorTopologyPackage
		if err := json.Unmarshal(document.Payload, &pack); err != nil {
			continue
		}
		for _, group := range pack.Groups {
			for _, item := range group.Ports {
				if name := strings.TrimSpace(item.Interface); name != "" {
					ports[server.resolveInterfaceID(ctx, name)] = true
				}
				if item.Bond != nil {
					for _, member := range item.Bond.Members {
						ports[server.resolveInterfaceID(ctx, member)] = true
					}
				}
			}
		}
	}
	return ports
}

// OrchestratorMirrorProvider exposes the desired SPAN generation and the VPP
// interface names whose sessions must be disabled first. The Orchestrator
// runtime is built before the HTTP server, so main installs this callback after
// NewServer; every transparent apply then converges the mirror in the same VPP
// operation as the plugin generation.
func (server *Server) OrchestratorMirrorProvider() func(context.Context) (*vpp.MirrorConfig, []string, error) {
	return func(ctx context.Context) (*vpp.MirrorConfig, []string, error) {
		mirror, err := server.runtimeMirrorConfig(ctx)
		if err != nil {
			return nil, nil, err
		}
		cleanup, err := server.mirrorCleanupInterfaces(ctx)
		if err != nil {
			return nil, nil, err
		}
		return mirror, cleanup, nil
	}
}

// interfaceStoredItem finds the persisted interface document by product id or
// resolved Linux name. Mirror validation runs on a PATCH payload, so the role
// and bond identity can only come from the store.
func (server *Server) interfaceStoredItem(ctx context.Context, id, linuxName string) map[string]any {
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return nil
	}
	for _, item := range items {
		if truthy(item["deleted"]) {
			continue
		}
		for _, key := range []string{"id", "interface_id", "name", "system_name"} {
			candidate := strings.TrimSpace(stringField(item, key))
			if candidate == "" {
				continue
			}
			if strings.EqualFold(candidate, id) || (linuxName != "" && strings.EqualFold(candidate, linuxName)) {
				return item
			}
		}
	}
	return nil
}

// mirrorCleanupInterfaces lists every VPP interface a stale SPAN session could
// still name. Disabling a session that does not exist is a no-op (`?set`), and
// the inventory is small, so this also covers a destination/source that was
// retired by "取消镜像" (whose document no longer lists the old sources).
func (server *Server) mirrorCleanupInterfaces(ctx context.Context) ([]string, error) {
	// This list is the union of every host port and every stored interface. It
	// only changes when a port appears, disappears or is renamed, so a short
	// cache saves a full sysfs walk on every policy apply.
	server.mirrorCleanupMu.Lock()
	if len(server.mirrorCleanupNames) > 0 && time.Since(server.mirrorCleanupAt) < 5*time.Second {
		cached := append([]string(nil), server.mirrorCleanupNames...)
		server.mirrorCleanupMu.Unlock()
		return cached, nil
	}
	server.mirrorCleanupMu.Unlock()
	names := map[string]struct{}{}
	addLinux := func(linux string) {
		linux = strings.TrimSpace(linux)
		if linux == "" {
			return
		}
		names[vppInterfaceName(linux)] = struct{}{}
	}
	for _, item := range hostInterfaceInventory() {
		addLinux(nonEmpty(stringField(item, "name"), stringField(item, "id")))
	}
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if truthy(item["deleted"]) {
			continue
		}
		_, hasMirror := item["mirror"]
		sources := interfaceMirrorSourcesOf(item)
		if !hasMirror && len(sources) == 0 {
			continue
		}
		if id := strings.TrimSpace(nonEmpty(stringField(item, "interface_id"), stringField(item, "id"))); id != "" {
			addLinux(server.resolveInterfaceID(ctx, id))
		}
		for _, source := range sources {
			addLinux(server.resolveInterfaceID(ctx, source.Interface))
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	sort.Strings(result)
	server.mirrorCleanupMu.Lock()
	server.mirrorCleanupNames = append([]string(nil), result...)
	server.mirrorCleanupAt = time.Now()
	server.mirrorCleanupMu.Unlock()
	return result, nil
}

// mirrorRuntimeComponent reports every desired mirror session and whether the
// live `show interface span` table proves it. A down mirror port is a warning
// only: the source keeps forwarding and the mirrored copy is counted as a TX
// error, so the appliance must not be reported degraded for it.
func (server *Server) mirrorRuntimeComponent(ctx context.Context) (RuntimeComponentState, bool) {
	desired, err := server.runtimeMirrorConfig(ctx)
	if err != nil {
		return RuntimeComponentState{Name: "mirror", State: "degraded", Reason: err.Error()}, true
	}
	if desired == nil {
		return RuntimeComponentState{}, false
	}
	output, readErr := execCommandOutput(ctx, envOrDefault("LY_ROUTE_VPPCTL", "vppctl"), "show", "interface", "span")
	component := RuntimeComponentState{Name: "mirror", Available: true}
	destination := strings.TrimPrefix(desired.Destination, "lyroute-")
	warnings := []string{}
	sessions := []string{}
	for _, source := range desired.Sources {
		sourceName := strings.TrimPrefix(source.Interface, "lyroute-")
		sessions = append(sessions, fmt.Sprintf("%s -> %s (%s, %s)", sourceName, destination, source.Direction, mirrorL2Label(source.L2)))
	}
	if readErr != nil {
		component.State = "degraded"
		component.Available = false
		component.Reason = "VPP span readback failed: " + readErr.Error()
		return component, true
	}
	if matchErr := vpp.MirrorReadbackMatches(output, desired); matchErr != nil {
		component.State = "degraded"
		component.Available = false
		component.Reason = "镜像会话未生效：" + matchErr.Error()
		return component, true
	}
	if snapshot, ok := server.vppLinkSnapshotFor(ctx, desired.LinuxInterface, ""); ok && snapshot.Carrier == "down" {
		warnings = append(warnings, fmt.Sprintf("镜像口 %s 无载波：镜像帧计入 tx-error，源口转发不受影响", destination))
	}
	reason := fmt.Sprintf("%d 个镜像会话生效：%s", len(sessions), strings.Join(sessions, "；"))
	if len(warnings) > 0 {
		reason += "；告警：" + strings.Join(warnings, "；")
	}
	component.State = "running"
	component.Reason = reason
	return component, true
}

func mirrorL2Label(l2 bool) string {
	if l2 {
		return "l2"
	}
	return "l3"
}
