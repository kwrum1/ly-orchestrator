package httpapi

import (
	"context"
	"errors"

	"ly-route/backend/internal/runtime/apply"
	"ly-route/backend/internal/runtime/flow"
	serviceRuntime "ly-route/backend/internal/runtime/service"
	"ly-route/backend/internal/runtime/trafficpolicy"
	"ly-route/backend/internal/runtime/vpp"
)

func (server *Server) buildOrchestratorRuntimePlan(ctx context.Context, requestID string) (RuntimePlan, error) {
	flowIntent, hasFlowIntent, err := server.runtimeFlowIntent(ctx)
	if err != nil {
		return RuntimePlan{}, err
	}
	var compiledFlow flow.CompiledIntent
	if hasFlowIntent {
		compiledFlow, err = flow.CompileIntent(flowIntent)
		if err != nil {
			return RuntimePlan{}, err
		}
	}
	compiledPolicy, err := server.currentOrchestratorSecurityConfig(ctx)
	if err != nil {
		return RuntimePlan{}, err
	}

	interfaces := server.runtimeDataInterfaces(ctx)
	server.persistDataplaneInventory(ctx, interfaces)
	dataplanePolicy := server.dataplanePolicy(ctx)
	observation := server.observeDataplaneCapability(ctx, interfaces)
	nativePath := observation.NativePathRequest(server.managementInterfaceID(ctx), interfaces, server.now().UTC(), server.managementNetworkShared(ctx))
	for index := range nativePath.Assignments {
		port := server.displayInterfaceName(ctx, nativePath.Assignments[index].LinuxInterface)
		nativePath.Assignments[index].AFPacketConsent = server.allowsAFPacketFor(ctx, dataplanePolicy, port)
	}
	selectedPath, selectionErr := vpp.SelectNativePath(nativePath)
	dataplanePrepared := false
	if composition, ok := server.gatewayTransaction.(apply.ProductionGatewayComposition); ok {
		dataplanePrepared = composition.HasDataplaneController()
	}
	gatewayPlan := vpp.Plan{RequestID: requestID, NativePath: nativePath, DataplanePrepared: dataplanePrepared, Flow: compiledFlow, Policy: compiledPolicy}
	operations, buildErr := vpp.BuildOperations(gatewayPlan)
	dataplaneState := "native_ready"
	var dataplaneProof []vpp.PrerequisiteResult
	var dataplaneDegradations []vpp.Degradation
	var afRequiredInterfaces []string
	afRequiredMessage := ""
	var warnings []string
	if selectionErr == nil {
		dataplaneDegradations = append(dataplaneDegradations, selectedPath.Degradations...)
		dataplaneProof = selectedPath.Prerequisites
	}
	if observation.Degraded {
		warnings = append(warnings, "dataplane capability degraded ("+string(observation.Source)+"): "+observation.Reason)
	}
	if buildErr != nil {
		var afRequired *vpp.AFFallbackRequiredError
		var locked *vpp.DataplaneLockedError
		switch {
		case errors.As(buildErr, &afRequired):
			operations = nil
			dataplaneState = "af_fallback_required"
			afRequiredInterfaces = server.canonicalAFRequiredInterfaces(ctx, afRequired.Interfaces)
			afRequiredMessage = "AF_PACKET 兼容模式需要操作员明确确认：该模式仍由 VPP 转发，但性能受限，请确认后再应用。"
			warnings = append(warnings, buildErr.Error())
		case errors.As(buildErr, &locked):
			operations = nil
			dataplaneState = locked.Code()
			if capabilityObservationDegradesDataplane(observation, len(nativePath.Assignments)) {
				dataplaneState = "dataplane_degraded"
				dataplaneDegradations = append(dataplaneDegradations, vpp.Degradation{Name: "dataplane_capability_observation", Reason: observation.Reason, Impact: "dataplane_apply"})
			}
			dataplaneProof = locked.Prerequisites
			warnings = append(warnings, buildErr.Error())
		default:
			return RuntimePlan{}, buildErr
		}
	}

	artifacts := []serviceRuntime.RenderedArtifact{}
	if len(operations) > 0 {
		artifacts, err = serviceRuntime.RenderVPPOperations(operations)
		if err != nil {
			return RuntimePlan{}, err
		}
	}
	components := server.runtimeStatusComponents(ctx, artifacts, len(operations) > 0)
	return RuntimePlan{
		FlowIntent: flowIntent, CompiledFlow: compiledFlow, CompiledPolicy: compiledPolicy,
		ServiceArtifacts: summarizeRuntimeArtifacts(artifacts), RuntimeArtifacts: artifacts,
		VppOperations: operations, Components: components, Warnings: warnings,
		DataplaneState: dataplaneState, DataplanePolicy: dataplanePolicy, DataplaneProof: dataplaneProof,
		DataplaneDegradations: dataplaneDegradations, SelectedNativePath: selectedPath,
		AFRequiredInterfaces: afRequiredInterfaces, AFRequiredMessage: afRequiredMessage,
		CapabilitySource: string(observation.Source), GatewayPlan: gatewayPlan,
	}, nil
}

func (server *Server) currentOrchestratorSecurityConfig(ctx context.Context) (trafficpolicy.Config, error) {
	securityItems, err := server.desiredItems(ctx, "security_acl")
	if err != nil {
		return trafficpolicy.Config{}, err
	}
	objectGroupItems, err := server.desiredItems(ctx, "object_group")
	if err != nil {
		return trafficpolicy.Config{}, err
	}
	return trafficpolicy.CompileConfigWithDomainIPSet(nil, securityItems, objectGroupItems, nil)
}
