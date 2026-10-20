package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"ly-route/backend/internal/orchestrator"
	"ly-route/backend/internal/persistence"
	"ly-route/backend/internal/product"
)

const (
	orchestratorTopologyConfigResource = "orchestrator_topology"
	orchestratorPolicyConfigResource   = "orchestrator_policy"
)

func isOrchestratorConfigResource(resourceType string) bool {
	return resourceType == orchestratorTopologyConfigResource || resourceType == orchestratorPolicyConfigResource
}

// orchestratorTopologyPackage mirrors orchestrator.TopologyView.  The
// orchestrator input types intentionally have no JSON tags because they are an
// internal API; configuration packages use the public view schema.
type orchestratorTopologyPackage struct {
	SchemaVersion       int                         `json:"schema_version"`
	ManagementInterface string                      `json:"management_interface"`
	ManagementShared    bool                        `json:"management_shared,omitempty"`
	Interfaces          []orchestratorInterfacePack `json:"interfaces"`
	Groups              []orchestratorGroupPack     `json:"orchestration_groups"`
}

type orchestratorInterfacePack struct {
	Name string                `json:"name"`
	Role string                `json:"role"`
	Port string                `json:"port,omitempty"`
	Bond *orchestratorBondPack `json:"bond,omitempty"`
}

type orchestratorBondPack struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

type orchestratorGroupPack struct {
	Name  string                      `json:"name"`
	Ports []orchestratorGroupPortPack `json:"ports"`
}

type orchestratorGroupPortPack struct {
	Interface string                `json:"interface,omitempty"`
	Direction string                `json:"direction"`
	Bond      *orchestratorBondPack `json:"bond,omitempty"`
}

func (pack orchestratorTopologyPackage) input() orchestrator.TopologyInput {
	input := orchestrator.TopologyInput{
		SchemaVersion:       pack.SchemaVersion,
		ManagementInterface: pack.ManagementInterface,
		ManagementShared:    pack.ManagementShared,
		Interfaces:          make([]orchestrator.InterfaceInput, 0, len(pack.Interfaces)),
		Groups:              make([]orchestrator.GroupInput, 0, len(pack.Groups)),
	}
	for _, item := range pack.Interfaces {
		parsed := orchestrator.InterfaceInput{Name: item.Name, Role: orchestrator.Role(item.Role), Port: item.Port}
		if item.Bond != nil {
			parsed.Bond = &orchestrator.BondInput{Name: item.Bond.Name, Members: append([]string(nil), item.Bond.Members...)}
		}
		input.Interfaces = append(input.Interfaces, parsed)
	}
	for _, group := range pack.Groups {
		parsed := orchestrator.GroupInput{Name: group.Name, Ports: make([]orchestrator.DirectedPortInput, 0, len(group.Ports))}
		for _, port := range group.Ports {
			item := orchestrator.DirectedPortInput{Interface: port.Interface, Direction: orchestrator.Direction(port.Direction)}
			if port.Bond != nil {
				item.Interface = ""
				item.Bond = &orchestrator.BondInput{Name: port.Bond.Name, Members: append([]string(nil), port.Bond.Members...)}
			}
			parsed.Ports = append(parsed.Ports, item)
		}
		input.Groups = append(input.Groups, parsed)
	}
	return input
}

// orchestratorConfigResources returns the desired orchestrator state in the
// same shape as the public config package.  Both keys are always present so a
// snapshot can represent "no topology/policy" and a later restore can remove
// them.
func (server *Server) orchestratorConfigResources(ctx context.Context) (map[string][]json.RawMessage, error) {
	if server.profile.ID() != product.Orchestrator().ID() {
		return nil, nil
	}
	resources := map[string][]json.RawMessage{
		orchestratorTopologyConfigResource: {},
		orchestratorPolicyConfigResource:   {},
	}
	if server.orchestratorRepository == nil {
		return resources, nil
	}
	topology, _, err := server.orchestratorRepository.Snapshot(ctx)
	if err != nil && !errors.Is(err, orchestrator.ErrTopologyNotFound) {
		return nil, fmt.Errorf("export orchestrator topology: %w", err)
	}
	if err == nil {
		raw, marshalErr := json.Marshal(topology.View())
		if marshalErr != nil {
			return nil, fmt.Errorf("encode orchestrator topology: %w", marshalErr)
		}
		resources[orchestratorTopologyConfigResource] = []json.RawMessage{raw}
	}
	policyRepository, ok := server.orchestratorRepository.(orchestrator.PolicyRepository)
	if !ok {
		return resources, nil
	}
	policy, _, err := policyRepository.PolicySnapshot(ctx)
	if err != nil && !errors.Is(err, orchestrator.ErrPolicyNotFound) {
		return nil, fmt.Errorf("export orchestrator policy: %w", err)
	}
	if err == nil {
		raw, marshalErr := json.Marshal(policy.View())
		if marshalErr != nil {
			return nil, fmt.Errorf("encode orchestrator policy: %w", marshalErr)
		}
		resources[orchestratorPolicyConfigResource] = []json.RawMessage{raw}
	}
	return resources, nil
}

// validateOrchestratorConfigPackage checks a package against the current active
// orchestrator state before anything is persisted.  It also rejects a topology
// change that would invalidate the currently persisted policy.
func (server *Server) validateOrchestratorConfigPackage(ctx context.Context, payload ConfigPackagePayload) error {
	if server.profile.ID() != product.Orchestrator().ID() {
		return nil
	}
	topologyItems := payload.Resources[orchestratorTopologyConfigResource]
	policyItems := payload.Resources[orchestratorPolicyConfigResource]
	if len(topologyItems) > 1 || len(policyItems) > 1 {
		return fmt.Errorf("%w: orchestrator topology and policy must each contain at most one item", ErrInvalidConfigPackage)
	}
	if len(topologyItems) == 0 && len(policyItems) == 0 {
		return nil
	}
	topology, hasTopology, err := server.candidateOrchestratorTopology(ctx, topologyItems)
	if err != nil {
		return err
	}
	if len(policyItems) == 1 {
		if !hasTopology {
			return fmt.Errorf("%w: orchestrator policy requires a topology", ErrInvalidConfigPackage)
		}
		var input orchestrator.PolicyInput
		if err := json.Unmarshal(policyItems[0], &input); err != nil {
			return fmt.Errorf("%w: decode orchestrator policy: %v", ErrInvalidConfigPackage, err)
		}
		if _, err := orchestrator.ParsePolicy(topology, input); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidConfigPackage, err)
		}
		return nil
	}
	if len(topologyItems) == 1 {
		// The topology is changing while no policy is supplied.  The active
		// policy must remain valid for the new interface/group inventory.
		policyRepository, ok := server.orchestratorRepository.(orchestrator.PolicyRepository)
		if !ok {
			return nil
		}
		policy, _, policyErr := policyRepository.PolicySnapshot(ctx)
		if errors.Is(policyErr, orchestrator.ErrPolicyNotFound) {
			return nil
		}
		if policyErr != nil {
			return fmt.Errorf("read active orchestrator policy: %w", policyErr)
		}
		raw, marshalErr := json.Marshal(policy.View())
		if marshalErr != nil {
			return fmt.Errorf("encode active orchestrator policy: %w", marshalErr)
		}
		var input orchestrator.PolicyInput
		if err := json.Unmarshal(raw, &input); err != nil {
			return fmt.Errorf("decode active orchestrator policy: %w", err)
		}
		if _, err := orchestrator.ParsePolicy(topology, input); err != nil {
			return fmt.Errorf("%w: topology change invalidates the active policy: %v", ErrInvalidConfigPackage, err)
		}
	}
	return nil
}

func (server *Server) candidateOrchestratorTopology(ctx context.Context, items []json.RawMessage) (orchestrator.Topology, bool, error) {
	if len(items) == 1 {
		var pack orchestratorTopologyPackage
		if err := json.Unmarshal(items[0], &pack); err != nil {
			return orchestrator.Topology{}, false, fmt.Errorf("%w: decode orchestrator topology: %v", ErrInvalidConfigPackage, err)
		}
		topology, err := orchestrator.ParseTopology(pack.input())
		if err != nil {
			return orchestrator.Topology{}, false, fmt.Errorf("%w: %v", ErrInvalidConfigPackage, err)
		}
		return topology, true, nil
	}
	if server.orchestratorRepository == nil {
		return orchestrator.Topology{}, false, nil
	}
	topology, _, err := server.orchestratorRepository.Snapshot(ctx)
	if errors.Is(err, orchestrator.ErrTopologyNotFound) {
		return orchestrator.Topology{}, false, nil
	}
	if err != nil {
		return orchestrator.Topology{}, false, fmt.Errorf("read active orchestrator topology: %w", err)
	}
	return topology, true, nil
}

// orchestratorConfigDocuments converts packaged orchestrator state into the
// persisted topology/policy documents.  The validation pass above has already
// established that the pair is coherent.
func (server *Server) orchestratorConfigDocuments(ctx context.Context, payload ConfigPackagePayload, updatedAt time.Time) ([]persistence.ConfigDocument, error) {
	if server.profile.ID() != product.Orchestrator().ID() {
		return nil, nil
	}
	topologyItems := payload.Resources[orchestratorTopologyConfigResource]
	policyItems := payload.Resources[orchestratorPolicyConfigResource]
	if len(topologyItems) == 0 && len(policyItems) == 0 {
		return nil, nil
	}
	topology, hasTopology, err := server.candidateOrchestratorTopology(ctx, topologyItems)
	if err != nil {
		return nil, err
	}
	var documents []persistence.ConfigDocument
	if len(topologyItems) == 1 {
		raw, hash, marshalErr := persistence.MarshalPayload(topology.View())
		if marshalErr != nil {
			return nil, fmt.Errorf("encode orchestrator topology: %w", marshalErr)
		}
		documents = append(documents, persistence.ConfigDocument{ResourceType: orchestratorTopologyConfigResource, ResourceID: "active", Payload: raw, PayloadHash: hash, UpdatedAt: updatedAt})
	}
	if len(policyItems) == 1 {
		if !hasTopology {
			return nil, fmt.Errorf("%w: orchestrator policy requires a topology", ErrInvalidConfigPackage)
		}
		var input orchestrator.PolicyInput
		if err := json.Unmarshal(policyItems[0], &input); err != nil {
			return nil, fmt.Errorf("%w: decode orchestrator policy: %v", ErrInvalidConfigPackage, err)
		}
		policy, err := orchestrator.ParsePolicy(topology, input)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidConfigPackage, err)
		}
		raw, hash, err := persistence.MarshalPayload(policy.View())
		if err != nil {
			return nil, fmt.Errorf("encode orchestrator policy: %w", err)
		}
		documents = append(documents, persistence.ConfigDocument{ResourceType: orchestratorPolicyConfigResource, ResourceID: "active", Payload: raw, PayloadHash: hash, UpdatedAt: updatedAt})
	}
	return documents, nil
}

// ensureOrchestratorConfigResources preserves orchestrator state when restoring
// a snapshot created before the orchestrator resources were part of config
// packages.  New snapshots always carry both keys, including empty arrays.
func (server *Server) ensureOrchestratorConfigResources(ctx context.Context, payload *ConfigPackagePayload) error {
	if server.profile.ID() != product.Orchestrator().ID() {
		return nil
	}
	_, hasTopology := payload.Resources[orchestratorTopologyConfigResource]
	_, hasPolicy := payload.Resources[orchestratorPolicyConfigResource]
	if hasTopology && hasPolicy {
		return nil
	}
	resources, err := server.orchestratorConfigResources(ctx)
	if err != nil {
		return err
	}
	if payload.Resources == nil {
		payload.Resources = map[string][]json.RawMessage{}
	}
	if !hasTopology {
		payload.Resources[orchestratorTopologyConfigResource] = resources[orchestratorTopologyConfigResource]
	}
	if !hasPolicy {
		payload.Resources[orchestratorPolicyConfigResource] = resources[orchestratorPolicyConfigResource]
	}
	return nil
}
