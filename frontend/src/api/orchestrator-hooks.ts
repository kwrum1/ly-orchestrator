import { useQuery } from "@tanstack/react-query";
import { getJson } from "./client";
import { API } from "./endpoints";
import type {
  ConversionList,
  DataEnvelope,
  OrchestratorOperation,
  OrchestratorPolicy,
  PolicyEnvelope,
  ServiceChainCompileResult,
  TopologyEnvelope,
  UpgradeList,
} from "./types";

async function optionalJson<T>(path: string): Promise<T | null> {
  try {
    return await getJson<T>(path);
  } catch (error: any) {
    if (error?.status === 404) return null;
    throw error;
  }
}

export const orchKeys = {
  topology: ["orchestrator", "topology"] as const,
  policy: ["orchestrator", "policy"] as const,
  operation: ["orchestrator", "operation"] as const,
  upgrades: ["orchestrator", "upgrades"] as const,
  conversions: ["orchestrator", "conversions"] as const,
  dashboard: ["orchestrator", "telemetry", "dashboard"] as const,
  hits: ["orchestrator", "telemetry", "policy-hits"] as const,
  chainStatus: ["orchestrator", "service-chain", "status"] as const,
};

export function useOrchestratorTopology() {
  return useQuery<TopologyEnvelope | null>({
    queryKey: orchKeys.topology,
    queryFn: () => optionalJson<TopologyEnvelope>(API.orchestrator.topology),
    staleTime: 15_000,
  });
}

export function useOrchestratorPolicy() {
  return useQuery<PolicyEnvelope | null>({
    queryKey: orchKeys.policy,
    queryFn: () => optionalJson<PolicyEnvelope>(API.orchestrator.policy),
    staleTime: 15_000,
  });
}

export function useOrchestratorOperation() {
  return useQuery<{ operation: OrchestratorOperation }>({
    queryKey: orchKeys.operation,
    queryFn: () => getJson(API.orchestrator.operations),
    refetchInterval: 5_000,
    staleTime: 2_000,
  });
}

export function useUpgrades() {
  return useQuery<UpgradeList>({
    queryKey: orchKeys.upgrades,
    queryFn: () => getJson(API.orchestrator.upgrades),
    refetchInterval: 5_000,
    staleTime: 2_000,
  });
}

export function useConversions() {
  return useQuery<ConversionList>({
    queryKey: orchKeys.conversions,
    queryFn: () => getJson(API.orchestrator.conversions),
    refetchInterval: 5_000,
    staleTime: 2_000,
  });
}

export function useOrchestratorDashboard() {
  return useQuery<DataEnvelope<any>>({
    queryKey: orchKeys.dashboard,
    queryFn: () => getJson(API.telemetry.dashboard),
    refetchInterval: 10_000,
    staleTime: 5_000,
  });
}

export function usePolicyHits() {
  return useQuery<DataEnvelope<any[]>>({
    queryKey: orchKeys.hits,
    queryFn: () => getJson(API.telemetry.policyHits),
    refetchInterval: 15_000,
    staleTime: 5_000,
  });
}

export function useServiceChainStatus() {
  return useQuery<any | null>({
    queryKey: orchKeys.chainStatus,
    queryFn: () => optionalJson<any>(API.orchestrator.serviceChainStatus),
    refetchInterval: 20_000,
  });
}

export type { OrchestratorPolicy, PolicyEnvelope, ServiceChainCompileResult, TopologyEnvelope };
