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
  if (/af_fallback_required/i.test(raw)) return "有网卡需要使用兼容模式，请确认后再应用";
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

export type TakeoverKind = "native" | "compat" | "available" | "consent" | "unsupported" | "management";

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
  label: string;
  down: boolean;
  pending: boolean;
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
 * Map a port's takeover capability to operator wording. The label depends only
 * on takeover_state / takeover_class, never on link state.
 */
export function takeoverInfo(item: any): TakeoverInfo {
  const state = takeoverStateOf(item);
  const takeoverClass = takeoverClassOf(item);
  if (state === "taken_over") {
    if (takeoverClass === "compatibility") {
      return {
        kind: "compat",
        label: "已接管 · 兼容模式（性能受限）",
        tone: "warning",
        assignable: true,
        state,
        detail: "该网卡已使用兼容模式接管，吞吐低于高性能模式。",
      };
    }
    return {
      kind: "native",
      label: "高性能（原生）",
      tone: "success",
      assignable: true,
      state,
      detail: "该网卡已由数据面高性能接管。",
    };
  }
  if (state === "consent_required") {
    return {
      kind: "consent",
      label: "可接管 · 需确认兼容模式",
      tone: "warning",
      assignable: false,
      state,
      detail: "该网卡需要确认后才能使用兼容模式，吞吐会受限。",
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
  if (state === "not_taken_over") {
    return {
      kind: "available",
      label: "未接管",
      tone: "neutral",
      assignable: true,
      state,
      detail: "该网卡尚未完成接管；可由数据面接管后再分配给线路。",
    };
  }
  return {
    kind: "available",
    label: "可接管（高性能）",
    tone: "info",
    assignable: true,
    state,
    detail: "该网卡可由数据面高性能接管。",
  };
}

/** Link state is deliberately a separate, neutral secondary label. */
export function linkInfo(item: any): LinkInfo {
  const down = normalized(item?.link_state) === "down";
  // Physical disconnection is the strongest signal: a pending verification is
  // not actionable until the cable is back, so say 网线未连接 first.
  if (down) return { label: "网线未连接", down: true, pending: false };
  const verification = normalized(item?.verification);
  if (verification === "pending_link") {
    return { label: "等待链路", down: false, pending: true };
  }
  if (verification === "failed") {
    return { label: "验证未通过", down: false, pending: false };
  }
  const mbps = Number(item?.link_speed_mbps);
  if (Number.isFinite(mbps) && mbps > 0) {
    return {
      label: mbps >= 1000 ? `${Number((mbps / 1000).toFixed(2))} Gbps` : `${Math.round(mbps)} Mbps`,
      down: false,
      pending: false,
    };
  }
  const speed = String(item?.speed || "").trim();
  return { label: speed || "链路正常", down: false, pending: false };
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
  if (phase === "failed" || /failed|apply_failed|dataplane_locked|rejected|error/.test(status)) {
    return {
      tone: "neutral",
      label: "配置未生效",
      sidebarLabel: "需要处理",
      reason: applyFailureMessage(last),
    };
  }
  if (phase === "degraded" || status.includes("degraded")) {
    return {
      tone: "neutral",
      label: "部分配置未生效",
      sidebarLabel: "需要处理",
      reason: "部分配置尚未生效，请检查设备状态",
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
  return { tone: "success", label: "运行正常", sidebarLabel: "设备正常", reason: "配置已生效，网卡接管正常" };
}

export interface TakeoverSummary {
  native: number;
  compat: number;
  available: number;
  consent: number;
  unsupported: number;
  mixed: boolean;
}

export function takeoverSummary(items: any[]): TakeoverSummary {
  const summary: TakeoverSummary = { native: 0, compat: 0, available: 0, consent: 0, unsupported: 0, mixed: false };
  for (const item of items) {
    const kind = takeoverInfo(item).kind;
    if (kind === "native") summary.native += 1;
    else if (kind === "compat") summary.compat += 1;
    else if (kind === "consent") summary.consent += 1;
    else if (kind === "unsupported") summary.unsupported += 1;
    else summary.available += 1;
  }
  summary.mixed = summary.compat > 0;
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
