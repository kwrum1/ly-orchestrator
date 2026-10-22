import { useMemo, useState } from "react";
import { Activity, ArrowDownRight, ArrowUpRight, ChartNoAxesCombined, Cpu, Users } from "lucide-react";
import { useOnlineUsers, useTopSessions, useTrafficTrend } from "../../api/hooks";
import { useOrchestratorDashboard, usePolicyHits } from "../../api/orchestrator-hooks";
import { Badge, Button, Card, EmptyState, Metric, PageHeader, Skeleton } from "../../components/ui";
import { TrafficChart } from "../../components/TrafficChart";
import { formatBytes, formatRate, formatTime, text, timeAgo } from "../../lib/format";

const WINDOWS = [["realtime", "实时"], ["5m", "5分钟"], ["1h", "1小时"], ["24h", "24小时"]] as const;

export function OrchestratorTelemetryPage() {
  const [windowName, setWindowName] = useState("5m");
  const dashboard = useOrchestratorDashboard();
  const hits = usePolicyHits();
  const trend = useTrafficTrend(windowName);
  const online = useOnlineUsers();
  const top = useTopSessions();
  const data = dashboard.data?.data || {};
  const boundary = data.boundary_totals || {};
  const wan = boundary.wan || {};
  const lan = boundary.lan || {};
  const series = trend.data?.series?.logical_egresses || [];
  const onlineUsers = online.data?.data?.items || [];
  const connections = top.data?.data?.items || [];
  const hitItems = hits.data?.data || [];
  const degraded = dashboard.data?.data?.degraded || trend.data?.degraded || false;

  return (
    <div className="page">
      <PageHeader
        title="遥测"
        subtitle="单机 VPP 边界流量、策略命中与连接；不可用时显示降级原因"
        actions={<><div className="segmented">{WINDOWS.map(([key, label]) => <button key={key} type="button" className={windowName === key ? "is-active" : ""} onClick={() => setWindowName(key)}>{label}</button>)}</div><Button size="sm" onClick={() => { dashboard.refetch(); hits.refetch(); trend.refetch(); online.refetch(); top.refetch(); }}>刷新</Button></>}
      />
      {degraded && <div className="inline-warning">遥测降级：{text(dashboard.data?.data?.degraded_reason || trend.data?.degraded_reason || "VPP 遥测不可用")}</div>}

      <div className="metric-grid">
        <Metric label="WAN → LAN" value={formatBytes(wan.wan_to_lan?.bytes)} hint={text(wan.wan_to_lan?.rate_state, "边界总量")} tone="blue" icon={<ArrowDownRight size={15} />} />
        <Metric label="LAN → WAN" value={formatBytes(wan.lan_to_wan?.bytes)} hint={text(wan.lan_to_wan?.rate_state, "边界总量")} tone="violet" icon={<ArrowUpRight size={15} />} />
        <Metric label="策略命中" value={hits.isLoading ? "--" : Array.isArray(hitItems) ? hitItems.length : 0} hint="策略条目" tone="amber" icon={<Cpu size={15} />} />
        <Metric label="在线用户" value={online.isLoading ? "--" : onlineUsers.length} hint="邻居/租约" tone="green" icon={<Users size={15} />} />
        <Metric label="Top 连接" value={top.isLoading ? "--" : connections.length} hint="当前采样" tone="plain" icon={<Activity size={15} />} />
      </div>

      <Card title="边界流量趋势" subtitle={`${windowName} · ${trend.data?.sampling_interval_seconds || "--"}s`} actions={<Badge tone={trend.data?.degraded ? "warning" : "success"} dot>{trend.data?.degraded ? "降级" : "可用"}</Badge>}>
        {trend.isLoading ? <Skeleton rows={5} /> : series.length ? <TrafficChart series={series} direction="total" height={280} /> : <EmptyState title="暂无流量采样" description={trend.data?.degraded_reason || "VPP 遥测未返回 logical egress 采样。"} icon={<ChartNoAxesCombined size={22} />} />}
      </Card>

      <div className="orch-policy-grid">
        <Card title="策略命中" subtitle="来自 /telemetry/policy-hits">
          <div className="mini-table-wrap">
            <table className="data-table">
              <thead><tr><th>策略/对象</th><th>命中</th><th>字节</th><th>最近</th></tr></thead>
              <tbody>
                {(Array.isArray(hitItems) ? hitItems : []).map((item: any, index: number) => (
                  <tr key={item.id || index}><td className="mono">{text(item.id || item.name || item.policy_id, "—")}</td><td>{text(item.hits || item.hit_count || item.count, "—")}</td><td>{formatBytes(item.bytes)}</td><td className="muted">{timeAgo(item.last_hit_at || item.observed_at)}</td></tr>
                ))}
              </tbody>
            </table>
            {(!Array.isArray(hitItems) || !hitItems.length) && <EmptyState title="暂无策略命中" description={text(dashboard.data?.data?.degraded_reason, "策略遥测未返回数据。")} icon={<Cpu size={22} />} />}
          </div>
        </Card>

        <Card title="Top 连接" subtitle="来自 /telemetry/top-sessions">
          <div className="mini-table-wrap">
            <table className="data-table">
              <thead><tr><th>源</th><th>目的</th><th>协议</th><th>字节</th></tr></thead>
              <tbody>
                {connections.slice(0, 20).map((item: any, index: number) => (
                  <tr key={index}><td className="mono">{text(item.src_ip || item.source_ip)}</td><td className="mono">{text(item.dst_ip || item.destination_ip)}</td><td>{String(item.protocol || "—").toUpperCase()}</td><td>{formatBytes(item.bytes)}</td></tr>
                ))}
              </tbody>
            </table>
            {!connections.length && <EmptyState title="暂无连接采样" description="VPP 连接遥测不可用。" icon={<Activity size={22} />} />}
          </div>
        </Card>
      </div>

      <Card title="在线用户" subtitle="来自 /telemetry/online-users">
        <div className="mini-table-wrap">
          <table className="data-table">
            <thead><tr><th>IP</th><th>MAC</th><th>状态</th><th>连接</th><th>时间</th></tr></thead>
            <tbody>{onlineUsers.slice(0, 30).map((item: any, index: number) => <tr key={item.ip || index}><td className="mono">{text(item.ip || item.ip_address)}</td><td className="mono">{text(item.mac)}</td><td>{text(item.neighbor_state || item.online_status)}</td><td>{text(item.sessions || item.connection_count, "0")}</td><td className="muted">{timeAgo(item.last_traffic_time)}</td></tr>)}</tbody>
          </table>
          {!onlineUsers.length && <EmptyState title="暂无在线用户" description="邻居/租约遥测为空。" icon={<Users size={22} />} />}
        </div>
        <p className="muted">更新时间 {formatTime(dashboard.dataUpdatedAt ? new Date(dashboard.dataUpdatedAt).toISOString() : "")}</p>
      </Card>
    </div>
  );
}
