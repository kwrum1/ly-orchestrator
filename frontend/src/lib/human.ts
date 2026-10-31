// Human-facing wording for control-plane states.
//
// Raw control-plane identifiers are useful in logs and API responses, but must
// never leak into the normal UI. Keep the translation in this one module so the
// rule is testable and easy to audit.

const INTERNAL_DETAIL =
  /(dataplane_locked|af_fallback_required|runtime_capability_proof|desired_not_applied|apply_in_progress|failed prerequisites|prerequisite[s]?\b|requires_runtime_proof|transaction\s+runtime-|runtime-[0-9a-f]{8,}|snapshot-[0-9a-f]{8,})/gi;

// Machine identifiers must never reach a page. Keep the rule in one place so
// every page (and its tests) applies the same definition.
const INTERNAL_ID = /(?:snapshot|runtime|txn|transaction|req|request)[-_][a-z0-9][a-z0-9-]{5,}|[0-9a-f]{32}/i;

export function hasInternalId(value: unknown): boolean {
  return INTERNAL_ID.test(String(value ?? ""));
}

export function hasInternalDetail(value: unknown): boolean {
  const text = String(value ?? "");
  INTERNAL_DETAIL.lastIndex = 0;
  return INTERNAL_DETAIL.test(text) || hasInternalId(text);
}

/** Remove internal control-plane tokens from a free-text reason. */
export function sanitizeDetail(value: unknown, fallback = ""): string {
  const raw = String(value ?? "").trim();
  if (!raw) return fallback;
  // Reasons that mention internal states are not salvageable as operator copy;
  // show the caller's human fallback instead.
  if (hasInternalDetail(raw)) return fallback;
  return raw;
}

/**
 * Translate any thrown/returned control-plane message into short Chinese.
 * Technical detail stays in the API body; the UI only shows what the operator
 * can act on.
 */
export function humanErrorMessage(value: unknown, fallback = "操作失败，请稍后重试"): string {
  const raw = value instanceof Error ? value.message : String(value ?? "");
  if (!raw.trim()) return fallback;
  if (/af_fallback_required/i.test(raw)) return "有网卡需要使用兼容模式，系统会自动回落，无需手工确认";
  if (/takeover_not_selectable/i.test(raw)) return "接管模式由系统自动决定，界面不再提供手工切换";
  if (/dataplane_locked|runtime_capability_proof|failed prerequisites|desired_not_applied|apply_in_progress|requires_runtime_proof/i.test(raw)) {
    return "设备数据面未就绪，请检查网卡接管状态";
  }
  if (/timeout|timed out|超时/i.test(raw)) return "设备响应超时，请稍后重试";
  if (/unauthorized|authentication|登录已失效/i.test(raw)) return "登录状态已失效，请重新登录";
  if (/forbidden|admin role required/i.test(raw)) return "当前账号无权执行此操作";
  const cleaned = sanitizeDetail(raw, "");
  if (!cleaned) return fallback;
  return cleaned;
}

/** Read-only wording for the mode the backend selected. */
export const TAKEOVER_MODE_LABELS: Record<string, string> = {
  dpdk_uio: "高性能 · DPDK UIO",
  dpdk_vfio: "高性能 · DPDK VFIO",
  af_xdp_zero_copy: "高性能 · AF_XDP 零拷贝",
  af_xdp_copy: "AF_XDP 拷贝模式",
  rdma: "高性能 · RDMA",
  af_packet: "兼容模式（性能受限）",
};

export function takeoverModeLabel(mode?: string): string {
  const value = String(mode || "").toLowerCase();
  return TAKEOVER_MODE_LABELS[value] || (value ? value : "自动选择中");
}

export type TakeoverKind = "native" | "compat" | "repair" | "available" | "unsupported" | "management";

export interface TakeoverInfo {
  kind: TakeoverKind;
  label: string;
  tone: "success" | "warning" | "neutral" | "info";
  /** Whether this port can be assigned to a line / bond. Link state is irrelevant. */
  assignable: boolean;
  state: string;
  detail: string;
}

export interface LinkInfo {
  /** Combined label for sorting and non-chip consumers. */
  label: string;
  /** Port capability (NIC/PMD maximum rate). */
  capability: string;
  /** Negotiated link rate, 接口 DOWN for a down link, "—" when unknown. */
  actual: string;
  down: boolean;
  pending: boolean;
  detail: string;
}

function normalized(value: unknown): string {
  return String(value ?? "").toLowerCase().replace(/[\s-]+/g, "_");
}

function legacyTokens(item: any): string {
  return [item?.takeover, item?.takeover_mode, item?.active_path, item?.work_mode]
    .map((value) => {
      if (typeof value === "string") return value;
      if (value && typeof value === "object") {
        try {
          return JSON.stringify(value);
        } catch {
          return "";
        }
      }
      return "";
    })
    .join(" ")
    .toLowerCase();
}

function takeoverClassOf(item: any): "native" | "compatibility" | "" {
  const explicit = normalized(item?.takeover_class || item?.takeover_mode);
  if (/compat|fallback/.test(explicit)) return "compatibility";
  if (/native|high|direct/.test(explicit)) return "native";
  const tokens = legacyTokens(item);
  if (/af[_-]?packet|compat/.test(tokens)) return "compatibility";
  if (/(dpdk|af[_-]?xdp|rdma|vpp|native)/.test(tokens)) return "native";
  return "";
}

function hasTakeoverContract(item: any): boolean {
  if (!item) return false;
  if (item.takeover_state || item.takeover_mode || item.takeover_class) return true;
  if (typeof item.takeover === "string" && item.takeover.trim()) return true;
  if (item.takeover && typeof item.takeover === "object" && Object.keys(item.takeover).length) return true;
  return legacyTokens(item).trim().length > 0;
}

function takeoverStateOf(item: any): string {
  const explicit = normalized(item?.takeover_state);
  if (explicit) return explicit;
  const tokens = legacyTokens(item);
  if (/unsupported|not_supported/.test(tokens)) return "unsupported";
  if (/consent/.test(tokens)) return "consent_required";
  if (/available|ready/.test(tokens)) return "available";
  if (/taken_over|takenover|bound/.test(tokens)) return "taken_over";
  if (/af[_-]?packet|compat|dpdk|af[_-]?xdp|rdma/.test(tokens)) return "taken_over";
  // Capability-first fallback: without an explicit state, assume the port is
  // assignable instead of making a down link look unsupported.
  return hasTakeoverContract(item) ? "available" : "not_taken_over";
}

/**
 * Map a port's takeover state to operator wording.
 *
 * There is no selection and no confirmation: the backend decides the mode from
 * the built-in rule library. The UI only reports what the appliance is doing.
 */
export function takeoverInfo(item: any): TakeoverInfo {
  const state = takeoverStateOf(item);
  const takeoverClass = takeoverClassOf(item);
  const transition = item?.takeover_transition;
  const repair =
    normalized(item?.takeover_display) === "needs_repair" ||
    state === "needs_repair" ||
    item?.needs_repair === true ||
    normalized(transition?.rollback_result) === "restore_failed";
  if (repair && !item?.management) {
    return {
      kind: "repair",
      label: "修复中",
      tone: "warning",
      assignable: false,
      state: "needs_repair",
      detail: sanitizeDetail(item?.reason, "上次接管失败且未能自动恢复，系统会在修复完成后重试。"),
    };
  }
  if (state === "management" || item?.management === true) {
    return {
      kind: "management",
      label: "管理口",
      tone: "neutral",
      assignable: false,
      state: "management",
      detail: "管理口由系统保留，不参与数据面接管。",
    };
  }
  if (state === "taken_over") {
    if (takeoverClass === "compatibility") {
      return {
        kind: "compat",
        label: "兼容模式（性能受限）",
        tone: "warning",
        assignable: true,
        state,
        detail: "系统自动选择了兼容模式，吞吐低于高性能模式；高性能接管会在条件满足时自动尝试。",
      };
    }
    return {
      kind: "native",
      label: "高性能",
      tone: "success",
      assignable: true,
      state,
      detail: takeoverModeLabel(item?.takeover_mode) + " · 系统自动接管，无需手工设置。",
    };
  }
  if (state === "stale") {
    return {
      kind: "available",
      label: "接管状态待确认",
      tone: "warning",
      assignable: true,
      state,
      detail: "接管记录与当前网卡不一致，系统会自动重新探测。",
    };
  }
  if (state === "unsupported" || state === "not_supported") {
    return {
      kind: "unsupported",
      label: "不支持接管",
      tone: "neutral",
      assignable: false,
      state,
      detail: "该网卡暂不支持接管，不能分配给线路或聚合组。",
    };
  }
  return {
    kind: "available",
    label: "待接管",
    tone: "info",
    assignable: true,
    state: state || "available",
    detail: "系统会自动尝试高性能接管；失败时自动回落到兼容模式。",
  };
}

/**
 * Link display contract: two rates are always kept apart.
 *
 *   - capability: the NIC/PMD maximum (grey), e.g. igb 1 Gbps;
 *   - actual: the live negotiated rate (amber).
 *
 * A down link (VPP carrier down or admin down) is an interface state, not a
 * cable/carrier explanation: it renders "接口 DOWN". An up link without a
 * negotiated rate renders "—"; unknown state also renders "—". The capability
 * stays visible in grey so an unplugged 1GbE port still shows its line rate.
 */
export function linkInfo(item: any): LinkInfo {
  const linkState = normalized(item?.link_state);
  const linkDown = linkState === "down";
  const linkUp = linkState === "up";
  const capability = capabilityLabel(item);
  const actualSpeed = speedLabel(item);
  const actual = linkDown ? "接口 DOWN" : linkUp && actualSpeed ? actualSpeed : "—";
  const parts = [capability, actual].filter(Boolean);
  return {
    label: parts.join(" · ") || "—",
    capability: capability || "—",
    actual,
    down: linkDown,
    pending: !linkDown && !actualSpeed && !capability,
    detail: `${capability ? `端口能力 ${capability}，` : ""}${linkDown ? "接口 DOWN" : linkUp ? (actualSpeed ? `实际速率 ${actualSpeed}` : "接口 UP，速率未知") : "链路状态未知"}`,
  };
}

function capabilityLabel(item: any): string {
  const mbps = Number(item?.link_speed_capability_mbps);
  if (!Number.isFinite(mbps) || mbps <= 0) return "";
  return mbpsLabel(mbps);
}

function mbpsLabel(mbps: number): string {
  return mbps >= 1000 ? `${Number((mbps / 1000).toFixed(2))} Gbps` : `${Math.round(mbps)} Mbps`;
}

function speedLabel(item: any): string {
  const mbps = Number(item?.link_speed_mbps);
  if (Number.isFinite(mbps) && mbps > 0) {
    return mbpsLabel(mbps);
  }
  const raw = String(item?.speed || "").trim();
  if (!raw) return "";
  // Backend rows sometimes carry "1000", "1000Mb/s", "2.5Gb/s" or "1 Gbps".
  const match = raw.match(/([0-9]+(?:\.[0-9]+)?)\s*([gGmM])/);
  if (match) {
    const value = Number(match[1]);
    const unit = match[2].toLowerCase();
    return unit === "g" ? `${value} Gbps` : `${Math.round(value)} Mbps`;
  }
  const numeric = Number(raw);
  if (Number.isFinite(numeric) && numeric > 0) {
    return mbpsLabel(numeric);
  }
  return "";
}

export function isManagementLike(item: any): boolean {
  if (!item) return false;
  if (item.management === true) return true;
  const role = String(item.gateway_role || item.role || item.mode_role?.gateway || "").toLowerCase();
  if (role === "management") return true;
  const names = [item.id, item.name, item.system_name].filter(Boolean).map((value) => String(value).toLowerCase());
  return names.includes("eth0");
}

const LOGICAL_KINDS = new Set(["lan_bridge", "bridge", "vlan", "wan", "proxy", "egress", "subscription", "logical", "bond", "bond_member"]);
const LOGICAL_ID = /^(lan-|lan_|lan-bridge-|br-|wan-|wan_|proxy-|egress-|bond-)/i;

/**
 * The NIC page only owns physical ports. Logical LAN/WAN objects (bridges,
 * proxy egresses, bonds) are managed by their own pages. A missing
 * system_name is *not* evidence that a port is logical.
 */
export function isLogicalPort(item: any): boolean {
  if (!item) return false;
  const kind = String(item?.kind || item?.type || "").toLowerCase();
  if (LOGICAL_KINDS.has(kind)) return true;
  const id = String(item?.id || "");
  const name = String(item?.name || "");
  return LOGICAL_ID.test(id) || LOGICAL_ID.test(name);
}

/** A port that carries data traffic and is expected to be taken over. */
export function isConfiguredDataPort(item: any): boolean {
  if (!item || item.__bond || isLogicalPort(item)) return false;
  if (item.management === true) return false;
  const role = String(item.gateway_role || item.role || item.mode_role?.gateway || "").toLowerCase();
  return role === "lan" || role === "wan";
}

/** Whether the dataplane has actually realized this port. */
export function portRealized(item: any): boolean {
  const info = takeoverInfo(item);
  if (info.kind === "native" || info.kind === "compat") return true;
  const virtual = String(item?.vpp_interface || item?.host_interface || "").trim();
  const runtime = normalized(item?.runtime_state);
  return Boolean(virtual) && /committed|applied|running|up|taken_over/.test(runtime);
}

export function snapshotDisplayName(snapshot: any, index = 0): string {
  const name = String(snapshot?.name || "").trim();
  if (name && !hasInternalId(name)) return name;
  return index === 0 ? "最近一次保存" : "系统配置快照";
}

export function snapshotKindLabel(value: unknown): string {
  const raw = String(value ?? "").toLowerCase();
  if (!raw || hasInternalId(raw)) return "快照";
  if (/manual|user|手动/.test(raw)) return "手动快照";
  if (/auto|runtime|rollback|scheduled|自动/.test(raw)) return "自动快照";
  return "快照";
}

export interface DeviceStatusSummary {
  tone: "success" | "warning" | "neutral" | "danger";
  label: string;
  sidebarLabel: string;
  reason: string;
}

/**
 * Single status line for the shell. It is normal only when the last apply is
 * settled and every configured data port has been realized; otherwise it
 * explains the actionable state in operator language.
 */
export function deviceStatusSummary(input: {
  phase?: string;
  lastApply?: any;
  ports?: any[];
  loading?: boolean;
  error?: boolean;
}): DeviceStatusSummary {
  const phase = String(input.phase || "").toLowerCase();
  const last = input.lastApply || {};
  const status = String(last.status || last.runtime_state || "").toLowerCase();
  const outcome = String(last.outcome || "").toLowerCase();
  const applying = phase === "applying" || ["applying", "apply_in_progress", "in_progress"].includes(status);
  if (applying) {
    return { tone: "neutral", label: "配置应用中", sidebarLabel: "配置应用中", reason: "正在应用配置，请稍候" };
  }
  if (input.loading) {
    return { tone: "neutral", label: "读取中", sidebarLabel: "状态读取中", reason: "正在读取设备状态" };
  }
  if (input.error) {
    return { tone: "neutral", label: "状态未知", sidebarLabel: "状态未知", reason: "暂时无法读取设备状态" };
  }
  if (!last || !Object.keys(last).length) {
    return { tone: "neutral", label: "状态未知", sidebarLabel: "状态未知", reason: "尚未读取到应用记录" };
  }
  if (phase === "failed" || outcome === "failed" || /failed|apply_failed|dataplane_locked|rejected|error/.test(status)) {
    return {
      tone: "neutral",
      label: "配置未生效",
      sidebarLabel: "需要处理",
      reason: applyFailureMessage(last),
    };
  }
  if (phase === "unchanged" || outcome === "unchanged" || outcome === "skipped" || status === "unchanged" || status === "skipped") {
    return {
      tone: "success",
      label: "配置未变更",
      sidebarLabel: "配置未变更",
      reason: "配置与已应用版本一致，本次没有重新下发",
    };
  }
  if (phase === "degraded" || outcome === "partial" || last.partial || status.includes("degraded") || status === "partial") {
    return {
      tone: "warning",
      label: "部分成功",
      sidebarLabel: "部分应用",
      reason: "配置已应用，但存在降级步骤，请到系统状态查看降级原因",
    };
  }
  const committed = status === "committed" || status === "applied";
  if (!committed) {
    return { tone: "neutral", label: "配置未生效", sidebarLabel: "需要处理", reason: "配置尚未完成应用" };
  }
  const configured = (input.ports || []).filter(isConfiguredDataPort);
  const unrealized = configured.filter((item) => !portRealized(item));
  if (unrealized.length) {
    return {
      tone: "neutral",
      label: "需要处理",
      sidebarLabel: "需要处理",
      reason: `${unrealized.length} 张已配置网卡尚未完成接管，请检查网卡设置`,
    };
  }
  return { tone: "success", label: "运行正常", sidebarLabel: "设备正常", reason: "" };
}

export interface TakeoverSummary {
  native: number;
  compat: number;
  repair: number;
  available: number;
  unsupported: number;
  mixed: boolean;
}

export function takeoverSummary(items: any[]): TakeoverSummary {
  const summary: TakeoverSummary = { native: 0, compat: 0, repair: 0, available: 0, unsupported: 0, mixed: false };
  for (const item of items) {
    const kind = takeoverInfo(item).kind;
    if (kind === "native") summary.native += 1;
    else if (kind === "compat") summary.compat += 1;
    else if (kind === "repair") summary.repair += 1;
    else if (kind === "unsupported") summary.unsupported += 1;
    else summary.available += 1;
  }
  summary.mixed = summary.compat > 0 || summary.repair > 0;
  return summary;
}

/**
 * Detect the machine-readable af_fallback_required apply response/error and
 * return the affected ports. Returns null when the response is not that case.
 */
export function afFallbackPorts(value: any): string[] | null {
  if (!value) return null;
  const root = value?.body && typeof value.body === "object" ? value.body : value;
  const error = root?.error && typeof root.error === "object" ? root.error : {};
  const code = String(root?.code || error?.code || root?.status || value?.code || "");
  const message = String(root?.message || error?.message || value?.message || "");
  if (!/af_fallback_required/i.test(`${code} ${message}`)) return null;
  const candidates = [
    root?.ports,
    root?.affected_ports,
    root?.af_ports,
    root?.details?.ports,
    error?.ports,
    error?.details?.ports,
    root?.data?.ports,
    root?.context?.ports,
  ];
  const ports: string[] = [];
  for (const candidate of candidates) {
    if (Array.isArray(candidate)) {
      for (const port of candidate) {
        const name = typeof port === "string" ? port : port?.name || port?.id || port?.interface;
        if (name && !ports.includes(String(name))) ports.push(String(name));
      }
    }
  }
  return ports;
}

export function applyFailureMessage(result: any): string {
  const status = String(result?.status || result?.runtime_state || result?.code || "").toLowerCase();
  if (status === "dataplane_locked" || hasInternalDetail(result?.reason)) {
    return "设备数据面未就绪，请检查网卡接管状态";
  }
  return humanErrorMessage(result?.reason || result?.message, "配置应用失败，请稍后重试");
}
