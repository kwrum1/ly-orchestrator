package httpapi

import (
	"context"
	"fmt"
	"strings"
)

// validateInterfaceLineRole rejects writing a LAN/WAN line onto a port whose
// stored role does not match. The installer dialog used to offer every data
// port, so an operator could create a LAN line on a WAN port (or the reverse)
// and silently change that port's role as a side effect.
func (server *Server) validateInterfaceLineRole(ctx context.Context, payload map[string]any) error {
	role, _ := interfaceDesiredRole(payload)
	if role != "lan" && role != "wan" {
		return nil
	}
	lineField := strings.TrimSpace(stringField(payload, "display_name"))
	if lineField == "" {
		lineField = strings.TrimSpace(nonEmpty(stringField(payload, "cidr"), stringField(payload, "address")))
	}
	if lineField == "" {
		return nil
	}
	id := strings.TrimSpace(nonEmpty(stringField(payload, "interface_id"), stringField(payload, "id")))
	if id == "" {
		return nil
	}
	items, err := server.desiredItems(ctx, "interface")
	if err != nil {
		return err
	}
	for _, item := range items {
		itemID := strings.TrimSpace(nonEmpty(stringField(item, "interface_id"), stringField(item, "id")))
		if itemID != id {
			continue
		}
		storedRole, _ := interfaceDesiredRole(item)
		if storedRole == role {
			return nil
		}
		// Re-purposing an assigned data port (LAN<->WAN) in one dialog is an
		// established flow and stays allowed. Only an unassigned or management
		// port must be marked with the target role first: those are the ports
		// the dialog offered even though nothing had selected them.
		if storedRole == "lan" || storedRole == "wan" {
			return nil
		}
		return fmt.Errorf("interface %s is not marked as %s yet (current role %q); set its role in 网卡设置 first, then create the line", id, strings.ToUpper(role), storedRole)
	}
	return nil
}
