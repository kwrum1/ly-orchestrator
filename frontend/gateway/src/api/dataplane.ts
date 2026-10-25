import { useQuery } from "@tanstack/react-query";
import { getJson, putJson } from "./client";
import { API } from "./endpoints";
import type { DataplanePolicy } from "./types";

/**
 * Dataplane fallback consent.
 *
 * INTEGRATION POINT: the backend dataplane branch is adding a small GET/PUT
 * setting (a dataplane_policy-style resource) that records whether the operator
 * has explicitly allowed AF_PACKET compatibility mode. The UI assumes:
 *
 *   GET /api/v1/dataplane/policy   -> { af_packet_fallback: boolean, ports?: string[] }
 *   PUT /api/v1/dataplane/policy   <- { af_packet_fallback: boolean, ports?: string[] }
 *
 * The route lives in api/endpoints.ts (`API.dataplane.policy`). If the final
 * route or field name differs, adjust only this file and endpoints.ts.
 */
export function policyAllowsFallback(policy?: DataplanePolicy | null): boolean {
  if (!policy) return false;
  return Boolean(policy.af_packet_fallback ?? policy.af_fallback ?? policy.compatible_mode);
}

export async function setAfFallbackConsent(enabled: boolean, ports: string[] = []): Promise<DataplanePolicy> {
  return putJson<DataplanePolicy>(API.dataplane.policy, { af_packet_fallback: enabled, ports });
}

export function useDataplanePolicy() {
  return useQuery<DataplanePolicy>({
    queryKey: ["dataplane", "policy"],
    queryFn: async () => {
      try {
        return await getJson<DataplanePolicy>(API.dataplane.policy);
      } catch {
        // Older builds do not expose the policy endpoint yet. Treat that as
        // "compatibility mode not allowed" rather than blocking the NIC page.
        return { af_packet_fallback: false } as DataplanePolicy;
      }
    },
    staleTime: 15_000,
    retry: false,
  });
}
