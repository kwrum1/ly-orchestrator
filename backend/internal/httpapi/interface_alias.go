package httpapi

import (
	"context"
	"strings"
)

// interfaceDocumentAliases returns the other keys an interface document may be
// stored under for the same port: the kernel netdev name (the canonical document
// id) and the product id. A PATCH that arrives with one spelling must still merge
// the document written with the other.
func (server *Server) interfaceDocumentAliases(ctx context.Context, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	seen := map[string]bool{}
	aliases := make([]string, 0, 3)
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || strings.EqualFold(candidate, value) || seen[strings.ToLower(candidate)] {
			return
		}
		seen[strings.ToLower(candidate)] = true
		aliases = append(aliases, candidate)
	}
	add(server.resolveInterfaceID(ctx, value))
	// The canonical product id is stored by Plan B; a lookup by any other
	// spelling must still find it, and a lookup by the product id must find a
	// legacy document keyed by the kernel name.
	add(value)
	if canonical := server.canonicalInterfaceReference(ctx, value); isProductInterfaceID(canonical) {
		add(canonical)
		add(server.resolveInterfaceID(ctx, canonical))
	}
	return aliases
}
