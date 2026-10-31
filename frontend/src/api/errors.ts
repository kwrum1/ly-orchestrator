import { ApiError } from "./client";

/**
 * Structured control-plane errors.
 *
 * The backend keeps `message` machine-readable and adds `referenced_by`,
 * `impact`, `cascade` and `requires_force` for write protection. The UI must
 * never flatten those into a toast: the operator needs to see what still uses
 * an object before deciding to change it.
 */
export interface DependencyReference {
  type: string;
  id: string;
  name?: string;
  field?: string;
  depth?: number;
  path?: string[];
  enabled?: boolean;
  suggestion?: string;
  [key: string]: any;
}

export interface DependencyConflict {
  status: number;
  code: string;
  message: string;
  resource?: { type?: string; id?: string };
  referenced_by: DependencyReference[];
  impact: DependencyReference[];
  cascade: DependencyReference[];
  requires_force: boolean;
  identity_change: boolean;
  suggestion?: string;
  graph_generation?: number;
  content_hash?: string;
  raw: any;
}

export interface FieldIssue {
  field: string;
  message: string;
}

const DEPENDENCY_CODES = new Set(["referenced_by", "object_group_in_use", "interface_role_in_use"]);

/** Return the inner error object (`body.error`) when present. */
export function apiErrorBody(error: unknown): any {
  const candidate: any = error;
  const body = candidate?.body ?? candidate?.response ?? candidate;
  if (!body || typeof body !== "object") return {};
  if (body.error && typeof body.error === "object") return body.error;
  return body;
}

export function apiErrorCode(error: unknown): string {
  const candidate: any = error;
  if (typeof candidate?.code === "string" && candidate.code) return candidate.code;
  const body = apiErrorBody(error);
  if (typeof body?.code === "string") return body.code;
  if (typeof candidate?.body?.error === "string") return candidate.body.error;
  return "";
}

export function apiErrorStatus(error: unknown): number {
  return error instanceof ApiError ? error.status : Number((error as any)?.status || 0);
}

/** The raw server message. Unlike ApiError.message it is not translated. */
export function rawErrorMessage(error: unknown): string {
  const body = apiErrorBody(error);
  const candidate: any = error;
  return String(body?.message || candidate?.body?.message || candidate?.message || "").trim();
}

/** Translation for any error, preserving an already-human fallback. */
export function displayErrorMessage(error: unknown, fallback = "操作失败，请稍后重试"): string {
  if (error instanceof ApiError && error.message) return error.message;
  const body = apiErrorBody(error);
  const raw = String(body?.message || (error as any)?.message || "").trim();
  return raw || fallback;
}

export function referencesOf(value: unknown): DependencyReference[] {
  if (!Array.isArray(value)) return [];
  return value
    .filter((item) => item && typeof item === "object")
    .map((item: any) => ({
      ...item,
      type: String(item.type || item.kind || item.resource_type || "object"),
      id: String(item.id || item.resource_id || item.reference_id || ""),
    }));
}

export function dependencyConflictOf(error: unknown): DependencyConflict | null {
  const body = apiErrorBody(error);
  const code = apiErrorCode(error);
  const references = referencesOf(body?.referenced_by ?? body?.references);
  const requiresForce = Boolean(body?.requires_force);
  if (!references.length && !requiresForce && !DEPENDENCY_CODES.has(code)) return null;
  const impact = referencesOf(body?.impact);
  return {
    status: apiErrorStatus(error),
    code: code || "referenced_by",
    message: String(body?.message || (error as any)?.message || "").trim(),
    resource: body?.resource,
    referenced_by: references,
    impact,
    cascade: referencesOf(body?.cascade),
    requires_force: requiresForce,
    identity_change: Boolean(body?.identity_change),
    suggestion: body?.suggestion,
    graph_generation: body?.graph_generation,
    content_hash: body?.content_hash,
    raw: body,
  };
}

function snatReason(text: string): string {
  if (/tenant VRF|NAT address already in use|proxy_snat_address_required/i.test(text)) {
    return "静态 WAN 接口地址已在 VRF 0，不能复用为代理出口的 SNAT。请在 snat_addresses 中填写该 WAN 网段内未被 DHCP、静态 NAT、端口映射或其他代理出口占用的保留地址。原始提示：" + text;
  }
  if (/not an IPv4 address/i.test(text)) return "地址格式无效，请输入 IPv4 地址";
  if (/is duplicated|listed more than once/i.test(text)) return "地址重复，请删除重复项";
  if (/is outside/i.test(text)) return "地址不在该 WAN 线路的网段内";
  if (/WAN interface address/i.test(text)) return "WAN 接口地址不能复用为代理 SNAT，请填写同网段内另一个未占用地址";
  if (/static mapping|port mapping|already owned/i.test(text)) {
    return "地址已被静态 NAT、端口映射或其他代理出口占用，请更换地址";
  }
  return text;
}

/**
 * Map a validation reason to the field that owns it. The backend currently
 * returns plain messages (not field objects) for 422, so the UI derives the
 * field from the documented wording.
 */
export function fieldIssueOf(error: unknown): FieldIssue | null {
  const raw = rawErrorMessage(error) || (error as any)?.message || "";
  const text = String(raw);
  if (!text) return null;
  if (/snat_addresses|\bSNAT\b/i.test(text)) return { field: "snat_addresses", message: snatReason(text) };
  if (/fallback_upstream_id/i.test(text)) return { field: "fallback_upstream_id", message: text };
  if (/proxy_egress_id|proxy egress/i.test(text)) return { field: "proxy_egress_id", message: text };
  if (/upstream_id|dns upstream/i.test(text)) return { field: "upstream_id", message: text };
  return null;
}

export function isDnsProxyUnavailable(error: unknown): boolean {
  const text = `${rawErrorMessage(error)} ${(error as any)?.message || ""}`;
  return /proxy\s*DNS|proxy_egress_dns|ProxyDNSEndpointUnavailable|代理 DNS/i.test(text);
}

export const DNS_PROXY_UNAVAILABLE_HINT =
  "当前版本没有代理 DNS 传输端点，不能保存“经代理出口解析”。可改为直连或拉黑；如必须走代理，请等待代理 DNS 端点实现后再启用。";
