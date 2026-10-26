import { humanErrorMessage } from "../lib/human";

export class ApiError extends Error {
  status: number;
  code: string;
  requestId?: string;
  body?: unknown;

  constructor(message: string, status: number, code = "request_failed", requestId?: string, body?: unknown) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.requestId = requestId;
    this.body = body;
  }
}

type RequestOptions = RequestInit & { timeoutMs?: number };

const DEFAULT_TIMEOUT = 15_000;

function emitUnauthorized() {
  window.dispatchEvent(new CustomEvent("lyroute:unauthorized"));
}

function errorFromResponse(status: number, text: string, headerRequestId?: string | null): ApiError {
  let body: any = null;
  try {
    body = text ? JSON.parse(text) : null;
  } catch {
    body = null;
  }
  const detail = body?.error;
  const requestId = detail?.request_id || headerRequestId || body?.request_id || undefined;
  const rawMessage = detail?.message || (status === 502 || status === 503 || status === 504
    ? "控制服务暂时不可用，请稍后重试"
    : `请求失败（HTTP ${status}）`);
  const message = humanErrorMessage(rawMessage, "请求失败，请稍后重试");
  return new ApiError(message, status, detail?.code || "request_failed", requestId, body);
}

export async function apiFetch<T>(path: string, options: RequestOptions = {}): Promise<T> {
  const { timeoutMs = DEFAULT_TIMEOUT, headers, ...init } = options;
  const controller = new AbortController();
  const timer = window.setTimeout(() => controller.abort(), timeoutMs);
  const requestId = `ui-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 8)}`;
  try {
    const response = await fetch(path, {
      ...init,
      credentials: "same-origin",
      signal: controller.signal,
      headers: {
        Accept: "application/json",
        "X-Request-ID": requestId,
        ...(headers || {}),
      },
    });
    const text = await response.text();
    if (response.status === 401) {
      if (!path.includes("/auth/session") && !path.includes("/auth/login")) emitUnauthorized();
      throw errorFromResponse(401, text, response.headers.get("X-Request-ID"));
    }
    if (!response.ok) {
      throw errorFromResponse(response.status, text, response.headers.get("X-Request-ID"));
    }
    if (!text) return undefined as T;
    try {
      return JSON.parse(text) as T;
    } catch {
      throw new ApiError("服务返回了无法解析的数据", response.status, "invalid_response", requestId, text);
    }
  } catch (error) {
    if (error instanceof ApiError) throw error;
    if (error instanceof DOMException && error.name === "AbortError") {
      throw new ApiError("请求超时，请检查设备状态后重试", 0, "timeout", requestId);
    }
    throw new ApiError("无法连接控制服务，请检查网络连接", 0, "network_error", requestId, error);
  } finally {
    window.clearTimeout(timer);
  }
}

export function getJson<T>(path: string, timeoutMs = DEFAULT_TIMEOUT) {
  return apiFetch<T>(path, { method: "GET", timeoutMs });
}

export function postJson<T>(path: string, body?: unknown, timeoutMs = DEFAULT_TIMEOUT) {
  return apiFetch<T>(path, {
    method: "POST",
    timeoutMs,
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

export function patchJson<T>(path: string, body: unknown, timeoutMs = DEFAULT_TIMEOUT) {
  return apiFetch<T>(path, {
    method: "PATCH",
    timeoutMs,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export function putJson<T>(path: string, body: unknown, timeoutMs = DEFAULT_TIMEOUT) {
  return apiFetch<T>(path, {
    method: "PUT",
    timeoutMs,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export function deleteJson<T>(path: string, timeoutMs = DEFAULT_TIMEOUT) {
  return apiFetch<T>(path, { method: "DELETE", timeoutMs });
}

export function uploadFile<T>(path: string, file: File, fields: Record<string, string> = {}, timeoutMs = 120_000) {
  const form = new FormData();
  Object.entries(fields).forEach(([key, value]) => form.append(key, value));
  form.append("file", file, file.name);
  return apiFetch<T>(path, { method: "POST", body: form, timeoutMs });
}

export async function downloadFile(path: string): Promise<Blob> {
  const response = await fetch(path, { credentials: "same-origin" });
  if (!response.ok) {
    const text = await response.text();
    throw errorFromResponse(response.status, text, response.headers.get("X-Request-ID"));
  }
  return response.blob();
}
