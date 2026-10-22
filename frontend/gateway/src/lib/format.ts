export function formatRate(value: unknown): string {
  if (value === undefined || value === null || value === "") return "--";
  const rate = Number(value);
  if (!Number.isFinite(rate) || rate < 0) return "--";
  if (rate >= 1_000_000_000) return `${(rate / 1_000_000_000).toFixed(2)} Gbps`;
  if (rate >= 1_000_000) return `${(rate / 1_000_000).toFixed(2)} Mbps`;
  if (rate >= 1_000) return `${(rate / 1_000).toFixed(2)} Kbps`;
  if (rate > 0 && rate < 10) return `${rate.toFixed(rate < 1 ? 2 : 1)} bps`;
  return `${Math.round(rate)} bps`;
}

export function formatBytes(value: unknown): string {
  if (value === undefined || value === null || value === "") return "--";
  const bytes = Number(value);
  if (!Number.isFinite(bytes) || bytes < 0) return "--";
  if (bytes >= 1_099_511_627_776) return `${(bytes / 1_099_511_627_776).toFixed(2)} TiB`;
  if (bytes >= 1_073_741_824) return `${(bytes / 1_073_741_824).toFixed(2)} GiB`;
  if (bytes >= 1_048_576) return `${(bytes / 1_048_576).toFixed(2)} MiB`;
  if (bytes >= 1_024) return `${(bytes / 1_024).toFixed(2)} KiB`;
  return `${Math.round(bytes)} B`;
}

export function formatDuration(secondsValue: unknown): string {
  const seconds = Number(secondsValue);
  if (!Number.isFinite(seconds) || seconds < 0) return "--";
  const days = Math.floor(seconds / 86400);
  const hours = Math.floor((seconds % 86400) / 3600);
  const minutes = Math.floor((seconds % 3600) / 60);
  if (days) return `${days}天 ${hours}小时`;
  if (hours) return `${hours}小时 ${minutes}分钟`;
  if (minutes) return `${minutes}分钟`;
  return `${Math.max(0, Math.floor(seconds))}秒`;
}

export function formatDateTime(value: unknown): string {
  if (!value) return "--";
  const date = new Date(String(value));
  if (Number.isNaN(date.valueOf())) return String(value);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function formatTime(value: unknown): string {
  if (!value) return "--";
  const date = new Date(String(value));
  if (Number.isNaN(date.valueOf())) return String(value);
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
}

export function timeAgo(value: unknown): string {
  if (!value) return "--";
  const timestamp = new Date(String(value)).valueOf();
  if (Number.isNaN(timestamp)) return String(value);
  const seconds = Math.max(0, Math.round((Date.now() - timestamp) / 1000));
  if (seconds < 60) return "刚刚";
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟前`;
  if (seconds < 86400) return `${Math.floor(seconds / 3600)} 小时前`;
  return `${Math.floor(seconds / 86400)} 天前`;
}

export function text(value: unknown, fallback = "--"): string {
  if (value === undefined || value === null || value === "") return fallback;
  return String(value);
}

export function number(value: unknown, fallback = "--"): string {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? parsed.toLocaleString("zh-CN") : fallback;
}

export function percent(value: unknown): string {
  const parsed = Number(value);
  return Number.isFinite(parsed) ? `${Math.round(parsed)}%` : "--";
}

export function uptime(value: unknown): string {
  return formatDuration(value);
}

export function joinNonEmpty(values: unknown[], separator = " · "): string {
  return values.filter((value) => value !== undefined && value !== null && String(value).trim() !== "").join(separator);
}

export function maskCidr(value: unknown): string {
  const raw = text(value, "");
  return raw || "--";
}

export function bytesFromBps(value: unknown): number {
  return Number(value) || 0;
}

export function splitLines(value: string): string[] {
  return value.split(/\r?\n/).map((line) => line.trim()).filter(Boolean);
}

export function splitTokens(value: string): string[] {
  return value.split(/[\s,]+/).map((line) => line.trim()).filter(Boolean);
}

export function classNames(...values: (string | false | null | undefined)[]): string {
  return values.filter(Boolean).join(" ");
}

export function isOnlineUser(item: any): boolean {
  const state = String(item?.online_status || item?.state || item?.neighbor_state || "").toLowerCase();
  return ["active", "online", "reachable", "up"].includes(state);
}

export function roleLabel(value: unknown): string {
  return ({ lan: "LAN", wan: "WAN", internal: "内部", external: "外部", management: "管理口" } as Record<string, string>)[String(value)] || text(value);
}

export function wanTypeLabel(value: unknown): string {
  const raw = String(value || "").toLowerCase();
  return ({ static: "固定 IPv4", static4: "固定 IPv4", static6: "固定 IPv6", dhcp4: "DHCP IPv4", dhcpv4: "DHCP IPv4", dhcp6: "DHCP IPv6", dhcpv6_pd: "DHCPv6-PD", pppoe: "PPPoE", proxy: "代理线路" } as Record<string, string>)[raw] || text(value);
}

export function stateLabel(value: unknown): string {
  const raw = String(value || "").toLowerCase();
  return ({
    running: "运行中",
    committed: "已应用",
    applied: "已应用",
    available: "可用",
    ok: "正常",
    healthy: "正常",
    degraded: "降级",
    unavailable: "不可用",
    failed: "失败",
    apply_failed: "应用失败",
    desired_not_applied: "待应用",
    pending: "待处理",
    stale: "数据过期",
    unknown: "未知",
    disabled: "已停用",
    enabled: "已启用",
    online: "在线",
    offline: "离线",
    reachable: "在线",
    up: "在线",
    down: "离线",
  } as Record<string, string>)[raw] || text(value);
}
