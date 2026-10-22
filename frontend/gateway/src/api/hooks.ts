import { useQuery, type UseQueryOptions } from "@tanstack/react-query";
import { API, TRAFFIC_POINTS } from "./endpoints";
import { getJson } from "./client";
import type {
  AuditEvent,
  AuthUser,
  Collection,
  DashboardSummary,
  DataEnvelope,
  DhcpServer,
  DnsPolicy,
  DnsUpstream,
  HealthResponse,
  InterfaceItem,
  ItemEnvelope,
  ObjectGroup,
  OnlineUser,
  PortMap,
  ProxyEgress,
  ProxyNode,
  ProxyStatus,
  ProxySubscription,
  RoutePolicy,
  RuntimeStatus,
  SessionItem,
  SessionResponse,
  StaticNat,
  TrafficControlRule,
  TrafficTrend,
  WanGroup,
  WanLink,
} from "./types";

export const qk = {
  session: ["session"] as const,
  health: ["health"] as const,
  summary: ["summary"] as const,
  telemetry: (kind: string, params?: unknown) => ["telemetry", kind, params] as const,
  resources: (kind: string) => ["resources", kind] as const,
  runtime: ["runtime"] as const,
  audit: ["audit"] as const,
};

export function useSession() {
  return useQuery<SessionResponse>({
    queryKey: qk.session,
    queryFn: () => getJson<SessionResponse>(API.auth.session),
    retry: false,
    staleTime: 30_000,
  });
}

export function useHealth() {
  return useQuery<HealthResponse>({
    queryKey: qk.health,
    queryFn: () => getJson<HealthResponse>(API.health),
    refetchInterval: 15_000,
    staleTime: 10_000,
  });
}

export function useSummary() {
  return useQuery<DashboardSummary>({
    queryKey: qk.summary,
    queryFn: () => getJson<DashboardSummary>(API.summary),
    refetchInterval: 10_000,
    staleTime: 5_000,
  });
}

export function useDashboardTelemetry() {
  return useQuery<DataEnvelope<any>>({
    queryKey: qk.telemetry("dashboard"),
    queryFn: () => getJson(API.telemetry.dashboard),
    refetchInterval: 10_000,
    staleTime: 5_000,
  });
}

export function useTrafficTrend(windowName: string) {
  const points = TRAFFIC_POINTS[windowName] || 300;
  return useQuery<TrafficTrend>({
    queryKey: qk.telemetry("traffic-trend", { windowName, points }),
    queryFn: () => getJson<TrafficTrend>(API.telemetry.trafficTrend(windowName, points)),
    refetchInterval: 10_000,
    staleTime: 5_000,
  });
}

export function useOnlineUsers() {
  return useQuery<DataEnvelope<{ items: OnlineUser[]; degraded?: boolean; degraded_reason?: string }>>({
    queryKey: qk.telemetry("online-users"),
    queryFn: () => getJson(API.telemetry.onlineUsers),
    refetchInterval: 10_000,
    staleTime: 5_000,
  });
}

export function useTopSessions() {
  return useQuery<DataEnvelope<{ items: SessionItem[]; degraded?: boolean; degraded_reason?: string }>>({
    queryKey: qk.telemetry("top-sessions"),
    queryFn: () => getJson(API.telemetry.topSessions),
    refetchInterval: 15_000,
    staleTime: 5_000,
  });
}

export function useSessions() {
  return useQuery<DataEnvelope<{ items: SessionItem[]; degraded?: boolean; degraded_reason?: string }>>({
    queryKey: qk.telemetry("sessions"),
    queryFn: () => getJson(API.telemetry.sessions),
    refetchInterval: 10_000,
    staleTime: 3_000,
  });
}

export function useTopDomains() {
  return useQuery<DataEnvelope<{ items: any[]; degraded?: boolean; degraded_reason?: string }>>({
    queryKey: qk.telemetry("top-domains"),
    queryFn: () => getJson(API.telemetry.topDomains),
    refetchInterval: 15_000,
    staleTime: 5_000,
  });
}

export function useInterfacesTelemetry() {
  return useQuery<DataEnvelope<InterfaceItem[]>>({
    queryKey: qk.telemetry("interfaces"),
    queryFn: () => getJson(API.telemetry.interfaces),
    refetchInterval: 5_000,
    staleTime: 2_000,
  });
}

export function useAudit() {
  return useQuery<any>({
    queryKey: qk.audit,
    queryFn: () => getJson(API.telemetry.audit),
    refetchInterval: 30_000,
    staleTime: 10_000,
  });
}

export function useRuntime() {
  return useQuery<RuntimeStatus>({
    queryKey: qk.runtime,
    queryFn: () => getJson<RuntimeStatus>(API.runtime.status, 25_000),
    refetchInterval: 20_000,
    staleTime: 8_000,
  });
}

export function useResource<T>(kind: string, path: string, options?: Partial<UseQueryOptions<Collection<T>>>) {
  return useQuery<Collection<T>>({
    queryKey: qk.resources(kind),
    queryFn: () => getJson<Collection<T>>(path),
    staleTime: 30_000,
    ...options,
  });
}

export function useInterfacesResource() {
  return useResource<InterfaceItem>("interfaces", API.resources.interfaces, { staleTime: 15_000 });
}

export function useWanLinks() {
  return useResource<WanLink>("wan-links", API.resources.wanLinks);
}

export function useWanGroups() {
  return useResource<WanGroup>("wan-groups", API.resources.wanGroups);
}

export function useRoutes() {
  return useResource<RoutePolicy>("routes", API.resources.routes);
}

export function usePortMaps() {
  return useResource<PortMap>("port-maps", API.resources.portMaps);
}

export function useStaticNat() {
  return useResource<StaticNat>("static-nat", API.resources.staticNat);
}

export function useDnsPolicies() {
  return useResource<DnsPolicy>("dns-policies", API.resources.dnsPolicies);
}

export function useDnsUpstreams() {
  return useResource<DnsUpstream>("dns-upstreams", API.resources.dnsUpstreams);
}

export function useDhcpServers() {
  return useResource<DhcpServer>("dhcp-servers", API.resources.dhcpServers);
}

export function useDhcpBindings() {
  return useResource<any>("dhcp-bindings", API.resources.dhcpBindings);
}

export function useObjectGroups() {
  return useResource<ObjectGroup>("object-groups", API.resources.objectGroups);
}

export function useTrafficControl() {
  return useResource<TrafficControlRule>("traffic-control", API.resources.trafficControl);
}

export function useUsers() {
  return useResource<AuthUser>("auth-users", API.auth.users);
}

export function useSecurityAcls() {
  return useResource<any>("security-acls", API.resources.securityAcls);
}

export function useSecurityIpMac() {
  return useResource<any>("security-ip-mac", API.resources.securityIpMac);
}

export function useSecurityThreat() {
  return useResource<any>("security-threat", API.resources.securityThreat);
}

export function useSecurityAttack() {
  return useResource<any>("security-attack", API.resources.securityAttack);
}

export function useProxyEgresses() {
  return useResource<ProxyEgress>("proxy-egresses", API.resources.proxyEgresses);
}

export function useProxyNodes() {
  return useResource<ProxyNode>("proxy-nodes", API.resources.proxyNodes);
}

export function useProxySubscriptions() {
  return useResource<ProxySubscription>("proxy-subscriptions", API.resources.proxySubscriptions);
}

export function useProxyStatus() {
  return useQuery<ProxyStatus>({
    queryKey: ["proxy-status"],
    queryFn: () => getJson<ProxyStatus>(API.resources.proxyStatus),
    refetchInterval: 15_000,
  });
}

export function useProxyLogs() {
  return useQuery<any>({
    queryKey: ["proxy-logs"],
    queryFn: () => getJson(API.resources.proxyLogs),
    refetchInterval: 15_000,
  });
}

export function usePPPoEStatus() {
  return useQuery<any>({
    queryKey: ["pppoe-status"],
    queryFn: () => getJson(API.resources.pppoeStatus),
    refetchInterval: 10_000,
  });
}

export function useSmartQos() {
  return useQuery<any>({
    queryKey: ["smart-qos"],
    queryFn: () => getJson(API.resources.smartQos),
    refetchInterval: 15_000,
  });
}

export function useFirmwareStatus() {
  return useQuery<any>({
    queryKey: ["firmware-status"],
    queryFn: () => getJson(API.firmware.status),
    refetchInterval: 10_000,
  });
}

export function useManagementNetwork() {
  return useQuery<any>({
    queryKey: ["management-network"],
    queryFn: () => getJson(API.management),
  });
}

export function itemsOf<T>(payload: Collection<T> | undefined): T[] {
  return Array.isArray(payload?.items) ? payload.items : [];
}

export function dataItems<T>(payload: DataEnvelope<{ items?: T[] }> | undefined): T[] {
  return Array.isArray(payload?.data?.items) ? payload.data.items : [];
}

export function capabilitiesOf(payload: { capabilities?: any[] } | undefined) {
  return Array.isArray(payload?.capabilities) ? payload.capabilities : [];
}
