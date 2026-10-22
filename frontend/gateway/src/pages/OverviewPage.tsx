import { useMemo, useState } from "react";
import { useNavigate } from "react-router-dom";
import {
  Activity,
  ArrowDownRight,
  ArrowUpRight,
  Cable,
  ChevronRight,
  CircleGauge,
  Clock3,
  Cpu,
  Globe2,
  HardDrive,
  ListFilter,
  MemoryStick,
  RefreshCw,
  Router,
  ServerCog,
  Users,
  Wifi,
} from "lucide-react";
import {
  useAudit,
  useDashboardTelemetry,
  useHealth,
  useInterfacesResource,
  useInterfacesTelemetry,
  useOnlineUsers,
  useProxyEgresses,
  useRuntime,
  useSummary,
  useTopDomains,
  useTopSessions,
  useTrafficTrend,
  useWanGroups,
  useWanLinks,
} from "../api/hooks";
import type { LogicalEgressSeries } from "../api/types";
import { Badge, Button, Card, EmptyState, ErrorState, Metric, PageHeader, ProgressBar, Skeleton } from "../components/ui";
import { Sparkline, TrafficChart, latestValue, seriesColor, totalLatest } from "../components/TrafficChart";
import { formatDateTime, formatDuration, formatRate, formatTime, isOnlineUser, percent, stateLabel, text, timeAgo } from "../lib/format";
import { useAuth } from "../state/providers";

const WINDOWS = [
  { key: "realtime", label: "实时" },
  { key: "5m", label: "5 分钟" },
  { key: "1h", label: "1 小时" },
  { key: "24h", label: "24 小时" },
];

function seriesLatest(item: LogicalEgressSeries) {
  const sample = item.samples?.[item.samples.length - 1];
  return {
    up: Number(sample?.upload_bps || 0),
    down: Number(sample?.download_bps || 0),
    total: Number(sample?.upload_bps || 0) + Number(sample?.download_bps || 0),
  };
}

function trendValues(item: LogicalEgressSeries) {
  return (item.samples || []).slice(-20).map((sample) => Number(sample.download_bps || 0) + Number(sample.upload_bps || 0));
}

function healthTone(value?: string) {
  const state = String(value || "").toLowerCase();
  if (["healthy", "ok", "up", "available"].includes(state)) return "success";
  if (["degraded", "stale", "warning"].includes(state)) return "warning";
  return "neutral";
}

export function OverviewPage() {
  const navigate = useNavigate();
  const { session } = useAuth();
  const [windowName, setWindowName] = useState("5m");
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const [topTab, setTopTab] = useState<"users" | "connections" | "domains">("users");

  const health = useHealth();
  const summary = useSummary();
  const dashboard = useDashboardTelemetry();
  const trend = useTrafficTrend(windowName);
  const online = useOnlineUsers();
  const sessions = useTopSessions();
  const domains = useTopDomains();
  const runtime = useRuntime();
  const audit = useAudit();
  const interfaces = useInterfacesTelemetry();
  const interfacesResource = useInterfacesResource();
  const wanLinks = useWanLinks();
  const wanGroups = useWanGroups();
  const proxyEgresses = useProxyEgresses();

  const series = trend.data?.series?.logical_egresses || [];
  const configuredEgresses = useMemo(() => [
    ...(wanLinks.data?.items || []).map((item) => ({ id: item.id, name: item.name || item.id, kind: "WAN", health: item.state || item.status || item.runtime_state, up: null as number | null, down: null as number | null })),
    ...(wanGroups.data?.items || []).map((item) => ({ id: item.id, name: item.name || item.id, kind: "WAN 群组", health: item.runtime_state, up: null as number | null, down: null as number | null })),
    ...(proxyEgresses.data?.items || []).filter((item) => item.id !== "proxy-egress-default").map((item) => ({ id: item.id, name: item.name || item.id, kind: "代理 WAN", health: item.runtime_state, up: null as number | null, down: null as number | null })),
  ], [wanLinks.data, wanGroups.data, proxyEgresses.data]);
  const displayEgresses = series.length ? series.map((item) => {
    const latest = seriesLatest(item);
    return { id: item.id, name: item.name || item.id, kind: item.kind === "wan_group" ? "WAN 群组" : item.kind === "proxy" ? "代理 WAN" : "直连 WAN", health: item.health || item.state, up: latest.up, down: latest.down };
  }) : configuredEgresses;

  const visibleSeries = series.filter((item) => !hidden.has(item.id));
  const down = totalLatest(visibleSeries, "download");
  const up = totalLatest(visibleSeries, "upload");
  const onlineUsers = online.data?.data?.items || [];
  const onlineCount = onlineUsers.length;
  const activeSessions = Number(dashboard.data?.data?.sessions ?? 0);
  const system = summary.data?.system || {};
  const load = system.load_average || {};
  const components = runtime.data?.components || [];
  const degradedComponents = components.filter((item) => item.available === false || item.state === "degraded");
  const interfaceItems = Array.isArray(interfaces.data?.data) ? interfaces.data?.data as any[] : [];
  const desiredInterfaces = interfacesResource.data?.items || [];
  const mergedInterfaces = desiredInterfaces.length ? desiredInterfaces.map((item) => {
    const telemetry = interfaceItems.find((candidate) => candidate.id === item.id || candidate.name === item.name);
    return { ...item, ...telemetry };
  }) : interfaceItems;

  const auditEvents = useMemo(() => {
    const payload = audit.data;
    const items = payload?.data?.items || payload?.items || (Array.isArray(payload) ? payload : []);
    return Array.isArray(items) ? items.slice(0, 8) : [];
  }, [audit.data]);

  const topUsers = onlineUsers.slice(0, 6);
  const topConnections = (sessions.data?.data?.items || []).slice(0, 6);
  const topDomains = (domains.data?.data?.items || []).slice(0, 6);

  const refreshAll = () => {
    [health, summary, dashboard, trend, online, sessions, domains, runtime, audit, interfaces, interfacesResource].forEach((query) => query.refetch());
  };

  return (
    <div className="page overview-page">
      <PageHeader
        title="总览"
        subtitle={`${system.platform || "Ly Route Gateway"} · 系统时间 ${formatDateTime(system.system_time)}`}
        actions={
          <>
            <Button size="sm" icon={<RefreshCw size={14} />} onClick={refreshAll}>刷新</Button>
            <Button size="sm" variant="primary" icon={<ServerCog size={14} />} onClick={() => navigate("/system/runtime")}>运行态</Button>
          </>
        }
        meta={
          <>
            <span className="meta-item"><i className={`status-dot status-dot-${health.data?.status === "ok" ? "success" : "warning"}`} />{health.data?.status === "ok" ? "运行正常" : "存在降级"}</span>
            <span className="meta-item"><Clock3 size={13} />运行 {formatDuration(system.uptime_seconds)}</span>
            <span className="meta-item"><HardDrive size={13} />版本 {health.data?.version || "--"}</span>
            <span className="meta-item"><Users size={13} />{session?.role === "readonly" ? "只读账号" : "管理员"}</span>
          </>
        }
      />

      {trend.isError && <ErrorState error={trend.error} onRetry={() => trend.refetch()} />}

      <div className="metric-grid">
        <Metric
          label="当前下行"
          value={trend.isLoading ? <span className="muted">读取中</span> : formatRate(down)}
          hint="接收速率"
          tone="blue"
          icon={<ArrowDownRight size={15} />}
          spark={<Sparkline values={visibleSeries.flatMap(trendValues).slice(-24)} color="#2563eb" />}
        />
        <Metric
          label="当前上行"
          value={trend.isLoading ? <span className="muted">读取中</span> : formatRate(up)}
          hint="发送速率"
          tone="violet"
          icon={<ArrowUpRight size={15} />}
          spark={<Sparkline values={visibleSeries.flatMap(trendValues).slice(-24)} color="#8b5cf6" />}
        />
        <Metric
          label="活动连接"
          value={dashboard.isLoading ? "--" : activeSessions.toLocaleString("zh-CN")}
          hint="当前会话"
          tone="amber"
          icon={<Activity size={15} />}
        />
        <Metric
          label="在线用户"
          value={online.isLoading ? "--" : onlineCount}
          hint="DHCP / 邻居"
          tone="green"
          icon={<Users size={15} />}
        />
        <Metric
          label="逻辑出口"
          value={displayEgresses.length}
          hint={`${displayEgresses.filter((item) => item.health === "healthy").length} 正常`}
          tone="plain"
          icon={<Router size={15} />}
        />
      </div>

      <div className="overview-main-grid">
        <Card
          className="span-8"
          title="WAN 流量趋势"
          subtitle={`出口上下行总流量 · 采样 ${trend.data?.sampling_interval_seconds || "--"}s`}
          actions={
            <div className="segmented">
              {WINDOWS.map((item) => (
                <button key={item.key} type="button" className={windowName === item.key ? "is-active" : ""} onClick={() => setWindowName(item.key)}>
                  {item.label}
                </button>
              ))}
            </div>
          }
        >
          <div className="chart-summary">
            <span>当前 <b>{formatRate(down)}</b></span>
            <span>上行 <b>{formatRate(up)}</b></span>
            <span>出口 <b>{displayEgresses.length}</b></span>
            <span className="muted">更新于 {formatTime(trend.dataUpdatedAt ? new Date(trend.dataUpdatedAt).toISOString() : "")}</span>
          </div>
          <div className="legend-row">
            {series.map((item, index) => (
              <button
                key={item.id}
                type="button"
                className={hidden.has(item.id) ? "legend-item is-hidden" : "legend-item"}
                onClick={() => {
                  setHidden((current) => {
                    const next = new Set(current);
                    if (next.has(item.id)) next.delete(item.id);
                    else next.add(item.id);
                    return next;
                  });
                }}
              >
                <i style={{ background: seriesColor(index) }} />
                <span>{item.name || item.id}</span>
                <em>{item.kind === "wan_group" ? "WAN 群组" : item.kind === "proxy" ? "代理" : "WAN"}</em>
              </button>
            ))}
            {!series.length && <span className="muted">{displayEgresses.length ? "遥测未返回出口" : "暂无 WAN 出口"}</span>}
          </div>
          {!series.length && displayEgresses.length > 0 && <div className="inline-warning">已配置 {displayEgresses.length} 条 WAN，流量遥测暂不可用；图表不展示推测数据。</div>}
          {trend.isLoading ? <Skeleton rows={5} /> : series.length ? <TrafficChart series={series} direction="total" hidden={hidden} height={270} /> : <EmptyState title="暂无流量采样" description="WAN 已配置但遥测未返回采样；请检查 VPP/遥测状态。" icon={<Activity size={22} />} />}
        </Card>

        <Card className="span-4" title="WAN 出口" subtitle="逻辑出口与当前速率" actions={<Button size="sm" variant="text" onClick={() => navigate("/traffic")}>查看全部</Button>}>
          <div className="egress-list">
            {displayEgresses.map((item, index) => {
              return (
                <button type="button" className="egress-row" key={item.id} onClick={() => navigate(series.length ? "/traffic" : "/wan")}>
                  <span className="egress-status">
                    <i style={{ background: seriesColor(index) }} />
                    <span>
                      <strong>{item.name || item.id}</strong>
                      <small>{item.kind}</small>
                    </span>
                  </span>
                  <span className="egress-rate">
                    <span><ArrowUpRight size={12} />{item.up === null ? "--" : formatRate(item.up)}</span>
                    <span><ArrowDownRight size={12} />{item.down === null ? "--" : formatRate(item.down)}</span>
                  </span>
                  <Badge tone={healthTone(item.health)} dot>{stateLabel(item.health || "unknown")}</Badge>
                  <ChevronRight size={14} className="muted" />
                </button>
              );
            })}
            {!displayEgresses.length && <EmptyState title="暂无 WAN 出口" description="请先在 LAN/WAN 页面创建 WAN 线路。" action={<Button size="sm" onClick={() => navigate("/wan")}>创建 WAN</Button>} icon={<Router size={22} />} />}
          </div>
        </Card>
      </div>

      <div className="overview-secondary-grid">
        <Card
          className="span-7"
          title="接口状态"
          subtitle="物理接口、角色与实时速率"
          actions={<Button size="sm" variant="text" onClick={() => navigate("/interfaces")}>接口设置</Button>}
        >
          <div className="port-grid">
            {mergedInterfaces.slice(0, 8).map((item) => (
              <button type="button" className="port-card" key={item.id || item.name} onClick={() => navigate("/interfaces")}>
                <span className="port-icon"><Cable size={16} /></span>
                <span className="port-name">{item.name || item.id}<small>{item.gateway_role ? stateLabel(item.gateway_role) : item.work_mode || "--"}</small></span>
                <span className="port-state">
                  <i className={`status-dot status-dot-${item.link_state === "up" ? "success" : "danger"}`} />
                  {item.speed || "--"}
                </span>
                <span className="port-rate">
                  <span>↓ {formatRate(item.rx_bps ?? item.stats?.rx_bps)}</span>
                  <span>↑ {formatRate(item.tx_bps ?? item.stats?.tx_bps)}</span>
                </span>
              </button>
            ))}
            {!mergedInterfaces.length && <EmptyState title="暂无接口数据" description="接口遥测暂不可用。" icon={<Cable size={22} />} />}
          </div>
        </Card>

        <Card className="span-5" title="系统健康" subtitle="资源使用与运行态组件" actions={<Badge tone={degradedComponents.length ? "warning" : "success"} dot>{degradedComponents.length ? `${degradedComponents.length} 个降级` : "正常"}</Badge>}>
          <div className="health-metrics">
            <div className="health-metric">
              <span><Cpu size={14} /> CPU</span>
              <strong>{percent(system.cpu_busy_percent)}</strong>
              <ProgressBar value={Number(system.cpu_busy_percent || 0)} tone={Number(system.cpu_busy_percent || 0) > 80 ? "red" : "blue"} />
            </div>
            <div className="health-metric">
              <span><MemoryStick size={14} /> 内存</span>
              <strong>{percent(system.memory_used_percent)}</strong>
              <ProgressBar value={Number(system.memory_used_percent || 0)} tone={Number(system.memory_used_percent || 0) > 85 ? "red" : "green"} />
            </div>
            <div className="health-metric">
              <span><CircleGauge size={14} /> 负载</span>
              <strong>{[load["1m"], load["5m"], load["15m"]].every((value) => Number.isFinite(Number(value))) ? `${Number(load["1m"]).toFixed(2)} / ${Number(load["5m"]).toFixed(2)} / ${Number(load["15m"]).toFixed(2)}` : "--"}</strong>
              <small className="muted">1 / 5 / 15 分钟</small>
            </div>
          </div>
          <div className="component-list">
            {components.slice(0, 6).map((component) => (
              <div className="component-row" key={component.name}>
                <span><i className={`status-dot status-dot-${component.available === false || component.state === "degraded" ? "warning" : "success"}`} />{component.name}</span>
                <span className="muted">{stateLabel(component.state)}</span>
              </div>
            ))}
            {!components.length && <p className="muted">运行态组件尚未读取。</p>}
          </div>
        </Card>
      </div>

      <div className="overview-bottom-grid">
        <Card
          className="span-7"
          title="Top 排行"
          subtitle="在线用户、连接与域名"
          actions={<Button size="sm" variant="text" onClick={() => navigate(topTab === "users" ? "/clients" : topTab === "connections" ? "/connections" : "/domains")}>查看全部</Button>}
        >
          <div className="subtabs">
            <button type="button" className={topTab === "users" ? "is-active" : ""} onClick={() => setTopTab("users")}>在线用户</button>
            <button type="button" className={topTab === "connections" ? "is-active" : ""} onClick={() => setTopTab("connections")}>连接</button>
            <button type="button" className={topTab === "domains" ? "is-active" : ""} onClick={() => setTopTab("domains")}>域名</button>
          </div>
          {topTab === "users" && (
            <div className="top-list">
              {topUsers.map((user, index) => (
                <button type="button" className="top-row" key={`${user.ip}-${index}`} onClick={() => navigate("/clients")}>
                  <span className="rank">{index + 1}</span>
                  <span className="top-main"><strong>{text(user.ip || user.ip_address)}</strong><small>{text(user.hostname || user.mac, "未知设备")}</small></span>
                  <span className="top-value">{formatRate(Number(user.rx_bps || 0) + Number(user.tx_bps || 0))}</span>
                  <Badge tone={isOnlineUser(user) ? "success" : "neutral"} dot>{isOnlineUser(user) ? "在线" : "离线"}</Badge>
                </button>
              ))}
              {!topUsers.length && <EmptyState title="暂无在线用户" icon={<Users size={22} />} />}
            </div>
          )}
          {topTab === "connections" && (
            <div className="top-list">
              {topConnections.map((item, index) => (
                <button type="button" className="top-row" key={index} onClick={() => navigate("/connections")}>
                  <span className="rank">{index + 1}</span>
                  <span className="top-main"><strong>{text(item.src_ip || item.source_ip)}</strong><small>{text(item.dst_ip || item.destination_ip)} · {text(item.protocol)}</small></span>
                  <span className="top-value">{Number(item.connection_count || item.connections || 1).toLocaleString("zh-CN")} 连接</span>
                </button>
              ))}
              {!topConnections.length && <EmptyState title="暂无连接数据" icon={<ListFilter size={22} />} />}
            </div>
          )}
          {topTab === "domains" && (
            <div className="top-list">
              {topDomains.map((item, index) => (
                <button type="button" className="top-row" key={index} onClick={() => navigate("/domains")}>
                  <span className="rank">{index + 1}</span>
                  <span className="top-main"><strong>{text(item.domain || item.name)}</strong><small>最近 {timeAgo(item.last_seen || item.last_access)}</small></span>
                  <span className="top-value">{Number(item.hits || item.queries || item.count || 0).toLocaleString("zh-CN")} 次</span>
                </button>
              ))}
              {!topDomains.length && <EmptyState title="暂无域名数据" icon={<Globe2 size={22} />} />}
            </div>
          )}
        </Card>

        <Card className="span-5" title="最近事件" subtitle="审计与配置动作" actions={<Button size="sm" variant="text" onClick={() => navigate("/system/runtime")}>查看运行态</Button>}>
          <div className="event-list">
            {auditEvents.map((event, index) => (
              <div className="event-row" key={`${event.id || index}`}>
                <i className={`event-dot event-${String(event.status || "").toLowerCase()}`} />
                <div>
                  <strong>{event.action || "事件"}<span className="muted"> · {event.actor || "system"}</span></strong>
                  <small>{event.resource || ""}{event.error ? ` · ${event.error}` : ""}</small>
                </div>
                <time>{timeAgo(event.timestamp)}</time>
              </div>
            ))}
            {!auditEvents.length && <EmptyState title="暂无审计事件" description="操作后事件会显示在这里。" icon={<Clock3 size={22} />} />}
          </div>
        </Card>
      </div>
    </div>
  );
}
