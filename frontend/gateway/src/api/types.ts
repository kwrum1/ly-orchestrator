export type AnyRecord = Record<string, any>;

export interface Capability {
  name: string;
  available: boolean;
  state: string;
  reason?: string;
}

export interface Collection<T> {
  items: T[];
  capabilities?: Capability[];
  request_id?: string;
}

export interface ItemEnvelope<T> {
  item: T;
  capabilities?: Capability[];
  request_id?: string;
}

export interface DataEnvelope<T> {
  data: T;
  capabilities?: Capability[];
  request_id?: string;
}

export interface SessionUser {
  username: string;
  role: "admin" | "readonly" | string;
  created_at?: string;
  password_change_required?: boolean;
}

export interface SessionResponse {
  session: SessionUser;
  password_change_required?: boolean;
  request_id?: string;
}

export interface HealthResponse {
  status: string;
  version?: string;
  degraded?: boolean;
  dependencies?: Capability[];
  request_id?: string;
}

export interface SystemSummary {
  cpu_busy_percent?: number;
  memory_total_bytes?: number;
  memory_available_bytes?: number;
  memory_used_bytes?: number;
  memory_used_percent?: number;
  load_average?: Record<string, number>;
  uptime_seconds?: number;
  system_time?: string;
  platform?: string;
}

export interface DashboardSummary {
  status?: string;
  degraded?: boolean;
  dependencies?: Capability[];
  system?: SystemSummary;
  capabilities?: Capability[];
}

export interface TrafficSample {
  timestamp: string;
  health?: string;
  download_bytes?: number;
  upload_bytes?: number;
  download_bps?: number | null;
  upload_bps?: number | null;
}

export interface LogicalEgressSeries {
  id: string;
  name: string;
  kind: string;
  health?: string;
  state?: string;
  fresh?: boolean;
  last_sample_at?: string;
  samples?: TrafficSample[];
}

export interface TrafficTrend {
  window?: string;
  points?: number;
  sampling_interval_seconds?: number;
  state?: string;
  degraded?: boolean;
  degraded_reason?: string;
  totals?: { download_bps?: number; upload_bps?: number };
  series?: { logical_egresses?: LogicalEgressSeries[] };
  request_id?: string;
}

export interface InterfaceStats {
  rx_bps?: number;
  tx_bps?: number;
  rx_pps?: number;
  tx_pps?: number;
  sessions?: number;
}

export interface InterfaceItem {
  id: string;
  name?: string;
  system_name?: string;
  display_name?: string;
  gateway_role?: string;
  role?: string;
  work_mode?: string;
  active_path?: string;
  addresses?: string[];
  address?: string;
  cidr?: string;
  mtu?: number;
  link_state?: string;
  admin_state?: string;
  speed?: string;
  mac?: string;
  rx_bps?: number;
  tx_bps?: number;
  rx_pps?: number;
  tx_pps?: number;
  sessions?: number;
  stats?: InterfaceStats;
  capability?: AnyRecord;
  degraded?: boolean;
  degraded_reason?: string;
  runtime_state?: string;
  vpp_interface?: string;
  host_interface?: string;
  mode_role?: AnyRecord;
  candidate_scopes?: string[];
  // Runtime takeover contract. The dataplane branch may expose these under
  // slightly different names; lib/human.ts accepts all of them.
  takeover?: string | AnyRecord;
  takeover_state?: string;
  takeover_mode?: string;
  takeover_class?: string;
  verification?: string;
  link_speed_mbps?: number;
  management?: boolean;
  reason?: string;
  bond?: string;
  bond_id?: string;
  member_of?: string;
  kind?: string;
  type?: string;
  interface_id?: string;
}

export interface InterfaceBond {
  id: string;
  name?: string;
  members?: string[];
  mode?: string;
  load_balance?: string;
  speed?: string;
  work_mode?: string;
  gateway_role?: string;
  link_state?: string;
  runtime_state?: string;
  [key: string]: any;
}

export interface DataplanePolicy {
  /** True after the operator explicitly allowed performance-limited AF_PACKET fallback. */
  af_packet_fallback?: boolean;
  af_fallback?: boolean;
  compatible_mode?: boolean;
  ports?: string[];
  [key: string]: any;
}

export interface WanLink {
  id: string;
  name?: string;
  interface_id?: string;
  type?: string;
  wan_type?: string;
  enabled?: boolean;
  cidr?: string;
  gateway?: string;
  mtu?: number;
  description?: string;
  username?: string;
  ipv4?: AnyRecord;
  ipv6?: AnyRecord;
  runtime_state?: string;
  status?: string;
  state?: string;
  [key: string]: any;
}

export interface WanGroup {
  id: string;
  name?: string;
  load_balance_mode?: string;
  mode?: string;
  hash_fields?: string;
  load_balance?: string;
  members?: string[];
  wan_members?: string[];
  weights?: Record<string, number>;
  member_weights?: { id: string; weight: number }[];
  connection_limits?: Record<string, number>;
  downlink_mbps?: Record<string, number>;
  primary_member?: string;
  backup_member?: string;
  runtime_state?: string;
  [key: string]: any;
}

export interface RoutePolicy {
  id: string;
  name?: string;
  priority?: number;
  enabled?: boolean;
  action?: "nat" | "route" | string;
  match?: {
    sources?: string[];
    destinations?: string[];
    src_port?: string;
    dst_port?: string;
    domain_group?: string;
    protocols?: string[];
    [key: string]: any;
  };
  egress?: string;
  next_hop?: string;
  full_cone?: boolean;
  runtime_state?: string;
  [key: string]: any;
}

export interface PortMap {
  id: string;
  name?: string;
  wan_link?: string;
  external_address?: string;
  external_port?: number;
  internal_host?: string;
  internal_port?: number;
  protocol?: string;
  description?: string;
  enabled?: boolean;
  hairpin?: boolean;
  full_cone?: boolean;
  session_count?: number | null;
  last_hit_at?: string | null;
  dataplane_observed?: boolean;
  runtime_state?: string;
  [key: string]: any;
}

export interface StaticNat {
  id: string;
  name?: string;
  wan_link?: string;
  external_address?: string;
  internal_host?: string;
  protocol?: string;
  enabled?: boolean;
  description?: string;
  [key: string]: any;
}

export interface DnsPolicy {
  id: string;
  name?: string;
  priority?: number;
  enabled?: boolean;
  policy?: {
    miss?: AnyRecord;
    rules?: AnyRecord[];
  };
  [key: string]: any;
}

export interface DnsUpstream {
  id: string;
  name?: string;
  servers?: string[];
  bootstrap_profile?: string;
  bootstrap_servers?: string[];
  wan_egress_id?: string;
  enabled?: boolean;
  [key: string]: any;
}

export interface DhcpServer {
  id: string;
  name?: string;
  interface_id?: string;
  subnet?: string;
  pools?: string[];
  routers?: string[];
  name_servers?: string[];
  lease_time_seconds?: number;
  description?: string;
  enabled?: boolean;
  [key: string]: any;
}

export interface DhcpBinding {
  id: string;
  mac?: string;
  ip?: string;
  hostname?: string;
  description?: string;
  enabled?: boolean;
  [key: string]: any;
}

export interface DhcpLease {
  id: string;
  ip_address?: string;
  mac?: string;
  hostname?: string;
  lease_end?: string;
  infinite?: boolean;
  state?: string;
  [key: string]: any;
}

export interface ObjectGroup {
  id: string;
  name?: string;
  kind?: "ip" | "domain" | string;
  description?: string;
  members?: string[];
  entries?: string[];
  source?: AnyRecord;
  source_entry_count?: number;
  enabled?: boolean;
  updated_at?: string;
  [key: string]: any;
}

export interface TrafficControlRule {
  id: string;
  rules?: AnyRecord[];
  enabled?: boolean;
  runtime_state?: string;
  hit_count?: number | null;
  hit_count_state?: string;
  [key: string]: any;
}

export interface SecurityResource {
  id: string;
  kind?: string;
  name?: string;
  description?: string;
  enabled?: boolean;
  priority?: number;
  action?: string;
  match?: AnyRecord;
  interface_id?: string;
  binding_mode?: string;
  unbound_behavior?: string;
  bindings?: { ip: string; mac: string }[];
  direction?: string;
  list_type?: string;
  entries?: string[];
  attack_type?: string;
  threshold_pps?: number;
  burst_packets?: number;
  enforcement_mode?: string;
  source_prefix?: string;
  destination_prefix?: string;
  [key: string]: any;
}

export interface ProxyEgress {
  id: string;
  name?: string;
  enabled?: boolean;
  underlay_wan_id?: string;
  node_id?: string;
  subscription_id?: string;
  description?: string;
  [key: string]: any;
}

export interface ProxyNode {
  id: string;
  name?: string;
  protocol?: string;
  uri?: string;
  address?: string;
  subscription_id?: string;
  enabled?: boolean;
  [key: string]: any;
}

export interface ProxySubscription {
  id: string;
  name?: string;
  url?: string;
  selection?: string;
  strategy?: string;
  top_n?: number;
  node_refs?: string[];
  enabled?: boolean;
  [key: string]: any;
}

export interface OnlineUser {
  ip?: string;
  ip_address?: string;
  mac?: string;
  hostname?: string;
  device_name?: string;
  online_status?: string;
  neighbor_state?: string;
  sessions?: number;
  connections?: number;
  connection_count?: number;
  rx_bps?: number;
  tx_bps?: number;
  rx_bytes?: number;
  tx_bytes?: number;
  online_duration_seconds?: number;
  last_traffic_time?: string;
  traffic_activity_state?: string;
  [key: string]: any;
}

export interface SessionItem {
  src_ip?: string;
  dst_ip?: string;
  translated_ip?: string;
  protocol?: string;
  src_port?: number;
  dst_port?: number;
  translated_port?: number;
  connection_count?: number;
  bytes?: number;
  age_seconds?: number;
  session_kind?: string;
  observed_at?: string;
  [key: string]: any;
}

export interface AuditEvent {
  id?: string;
  timestamp?: string;
  actor?: string;
  role?: string;
  resource?: string;
  action?: string;
  status?: string;
  error?: string;
  request_id?: string;
  [key: string]: any;
}

export interface RuntimeComponent {
  name: string;
  state?: string;
  available?: boolean;
  reason?: string;
  [key: string]: any;
}

export interface RuntimeApply {
  status?: string;
  runtime_state?: string;
  reason?: string;
  transaction_id?: string;
  applied_services?: string[];
  snapshot_hash?: string;
  applied_at?: string;
  components?: RuntimeComponent[];
  rollback?: AnyRecord;
  rollback_receipt?: AnyRecord;
  events?: AnyRecord[];
  [key: string]: any;
}

export interface RuntimeStatus {
  status?: string;
  components?: RuntimeComponent[];
  last_apply?: RuntimeApply;
  request_id?: string;
  error?: string;
}

export interface RuntimePreview {
  status?: string;
  request_id?: string;
  plan?: AnyRecord;
  [key: string]: any;
}

export interface FirmwareStatus {
  status?: string;
  staged?: boolean;
  [key: string]: any;
}

export interface ProxyStatus {
  service?: string;
  state?: string;
  available?: boolean;
  reason?: string;
  [key: string]: any;
}

export interface PPPoEStatus {
  status?: string;
  state?: string;
  reason?: string;
  peers?: AnyRecord[];
  [key: string]: any;
}

export interface ManagementNetwork {
  item?: AnyRecord;
  [key: string]: any;
}

export interface AuthUser {
  username: string;
  role: string;
  enabled?: boolean;
}

export interface TopologyPort {
  interface?: string;
  direction?: "lan_facing" | "wan_facing" | string;
  bond?: { name: string; members: string[] } | null;
}

export interface TopologyInterface {
  name: string;
  role: "lan" | "wan" | string;
  port?: string;
  bond?: { name: string; members: string[] } | null;
}

export interface OrchestrationGroup {
  name: string;
  ports: TopologyPort[];
}

export interface Topology {
  schema_version: number;
  management_interface: string;
  management_shared?: boolean;
  interfaces: TopologyInterface[];
  orchestration_groups: OrchestrationGroup[];
}

export interface TopologyEnvelope {
  item: Topology;
  checksum?: string;
}

export interface OrchestrationGroupEnvelope {
  item: OrchestrationGroup;
  checksum?: string;
}

export interface PolicyPortRange {
  start: number;
  end: number;
}

export interface PolicyMatch {
  sources: string[];
  destinations: string[];
  protocol: "any" | "tcp" | "udp" | "icmp" | "icmpv6" | string;
  source_ports?: PolicyPortRange[];
  destination_ports?: PolicyPortRange[];
}

export interface PolicyAction {
  kind: "via" | "direct" | "drop" | string;
  group?: string;
}

export interface PolicyRule {
  id: string;
  sequence: number;
  match: PolicyMatch;
  action: PolicyAction;
}

export interface PolicyGroup {
  id: string;
  position: number;
  rules: PolicyRule[];
}

export interface PolicyIPObject {
  id: string;
  prefixes: string[];
}

export interface OrchestratorPolicy {
  schema_version: number;
  ip_objects: PolicyIPObject[];
  policy_groups: PolicyGroup[];
  default: PolicyAction;
}

export interface PolicyEnvelope {
  item: OrchestratorPolicy;
  checksum?: string;
}

export interface OrchestratorOperation {
  name?: string;
  resource?: string;
  state?: string;
  started_at?: string;
  ended_at?: string;
}

export interface UpgradeVersion {
  product?: string;
  version?: string;
  commit?: string;
  suite?: string;
  arch?: string;
  upgrade_supported?: boolean;
}

export interface UpgradeTask {
  id: string;
  product?: string;
  from_version?: string;
  to_version?: string;
  state: string;
  package_name?: string;
  package_sha256?: string;
  package_size?: number;
  services?: string[];
  actor?: string;
  reason?: string;
  backup_id?: string;
  created_at?: string;
  updated_at?: string;
}

export interface UpgradeList {
  version?: UpgradeVersion;
  items: UpgradeTask[];
}

export interface ConversionPlanStep {
  id?: string;
  description?: string;
  reversible?: boolean;
}

export interface ConversionPlan {
  from_product?: string;
  to_product?: string;
  from_version?: string;
  to_version?: string;
  base_version?: string;
  dataplane_mode?: string;
  verify_packages?: string[];
  required_plugin?: string;
  config_migration?: string;
  preserve_database?: boolean;
  snapshot_required?: boolean;
  rollback_supported?: boolean;
  backup_required?: boolean;
  preserve_service_state?: boolean;
  service_units?: string[];
  steps?: ConversionPlanStep[];
  warnings?: string[];
}

export interface ConversionValidation {
  valid: boolean;
  issues?: string[];
  plan?: ConversionPlan;
}

export interface ConversionTask {
  id: string;
  state: string;
  from_product?: string;
  to_product?: string;
  from_version?: string;
  to_version?: string;
  base_version?: string;
  plan?: ConversionPlan;
  issues?: string[];
  snapshot_id?: string;
  service_state?: Record<string, string>;
  actor?: string;
  reason?: string;
  created_at?: string;
  updated_at?: string;
}

export interface ConversionList {
  items: ConversionTask[];
}

export interface ServiceChainBinding {
  group: string;
  wan_facing_next_hop: string;
  lan_facing_next_hop: string;
}

export interface ServiceChainFlow {
  source_ip: string;
  destination_ip: string;
  protocol: string;
  source_port?: number;
  destination_port?: number;
}

export interface ServiceChainCompileResult {
  path?: AnyRecord;
  chain?: AnyRecord;
  topology_checksum?: string;
  policy_checksum?: string;
}
