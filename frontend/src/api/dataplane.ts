import { useQuery } from "@tanstack/react-query";
import { getJson } from "./client";
import { API } from "./endpoints";
import type { DataplanePolicy } from "./types";

/**
 * Read-only dataplane policy.
 *
 * Takeover is automatic: the backend enumerates the physical data ports,
 * excludes the management port and selects the mode from its built-in rule
 * library. The UI has no write path for a takeover mode any more; this hook only
 * exists so diagnostics can show the persisted legacy fields.
 */
export function policyAllowsFallback(policy?: DataplanePolicy | null): boolean {
  if (!policy) return false;
  return Boolean(policy.af_packet_fallback ?? policy.af_fallback ?? policy.compatible_mode);
}

export function useDataplanePolicy() {
  return useQuery<DataplanePolicy>({
    queryKey: ["dataplane", "policy"],
    queryFn: async () => {
      try {
        return await getJson<DataplanePolicy>(API.dataplane.policy);
      } catch {
        return { af_packet_fallback: false } as DataplanePolicy;
      }
    },
    staleTime: 15_000,
    retry: false,
  });
}
