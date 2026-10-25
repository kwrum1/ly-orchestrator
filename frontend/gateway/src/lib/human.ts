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

export type TakeoverKind = "native" | "compat" | "none";

export interface TakeoverInfo {
  kind: TakeoverKind;
  label: string;
  tone: "success" | "warning" | "neutral";
  /** Advanced detail. Only rendered inside a details tooltip. */
  detail: string;
}

const COMPAT_DRIVER = /af[_-]?packet/i;
const NATIVE_DRIVER = /(dpdk|vfio|uio|af[_-]?xdp|rdma|vpp[_-]?native|vpp|native|hardware)/i;

function driverText(item: any): string {
  const direct = [item?.hook, item?.driver, item?.dataplane_hook, item?.dataplane_driver, item?.takeover_driver]
    .filter((value) => typeof value === "string" && value.trim());
  if (direct.length) return String(direct[0]).trim();
  const state = [item?.takeover, item?.takeover_state, item?.takeover_mode, item?.active_path, item?.work_mode]
    .filter((value) => typeof value === "string" && value.trim());
  return state.length ? String(state[0]).trim() : "";
}

/**
 * Map a port's runtime takeover state to operator wording.
 * Accepts several backend field spellings while the dataplane branch settles on
 * one canonical shape (takeover / takeover_state / driver / active_path).
 */
export function takeoverInfo(item: any): TakeoverInfo {
  const tokens = [item?.takeover, item?.takeover_state, item?.takeover_mode, item?.hook, item?.driver, item?.dataplane_hook, item?.dataplane_driver, item?.active_path, item?.work_mode]
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
    .join(" ");
  const driver = driverText(item);
  if (COMPAT_DRIVER.test(tokens)) {
    return {
      kind: "compat",
      label: "兼容模式（性能受限）",
      tone: "warning",
      detail: `高级信息：接管驱动 ${driver || "af_packet"}；吞吐低于高性能模式，CPU 占用更高。`,
    };
  }
  if (NATIVE_DRIVER.test(tokens)) {
    return {
      kind: "native",
      label: "高性能（原生）",
      tone: "success",
      detail: `高级信息：接管驱动 ${driver || "native"}。`,
    };
  }
  return {
    kind: "none",
    label: "未接管",
    tone: "neutral",
    detail: driver ? `高级信息：当前驱动 ${driver}，未接入数据面。` : "该网卡未接入数据面，暂不可分配给线路或聚合组。",
  };
}

export function takeoverSummary(items: any[]): { compat: number; native: number; none: number; mixed: boolean } {
  let compat = 0;
  let native = 0;
  let none = 0;
  for (const item of items) {
    const kind = takeoverInfo(item).kind;
    if (kind === "compat") compat += 1;
    else if (kind === "native") native += 1;
    else none += 1;
  }
  return { compat, native, none, mixed: compat > 0 && native + compat > 0 };
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
