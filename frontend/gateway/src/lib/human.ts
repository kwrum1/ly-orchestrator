// Human-facing wording for control-plane states.
//
// Raw control-plane identifiers are useful in logs and API responses, but must
// never leak into the normal UI. Keep the translation in this one module so the
// rule is testable and easy to audit.

const INTERNAL_DETAIL =
  /(dataplane_locked|af_fallback_required|runtime_capability_proof|desired_not_applied|apply_in_progress|failed prerequisites|prerequisite[s]?\b|requires_runtime_proof|transaction\s+runtime-|runtime-[0-9a-f]{8,}|snapshot-[0-9a-f]{8,})/gi;

export function hasInternalDetail(value: unknown): boolean {
  const text = String(value ?? "");
  INTERNAL_DETAIL.lastIndex = 0;
  return INTERNAL_DETAIL.test(text);
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
  return "available";
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
