import { useState } from "react";
import { ArrowDownRight, ArrowUpRight, RefreshCw, Activity } from "lucide-react";
import { useDashboardTelemetry, useOnlineUsers, useTrafficTrend } from "../api/hooks";
import { Badge, Button, Card, Metric, PageHeader, Skeleton, EmptyState } from "../components/ui";
import { TrafficChart, latestValue, seriesColor, totalLatest } from "../components/TrafficChart";
import { formatRate, formatTime } from "../lib/format";
import { sanitizeDetail } from "../lib/human";

const WINDOWS = [
  { key: "realtime", label: "实时" },
  { key: "5m", label: "5 分钟" },
  { key: "1h", label: "1 小时" },
  { key: "24h", label: "24 小时" },
];

export function TrafficPage() {
  const [windowName, setWindowName] = useState("5m");
  const [hidden, setHidden] = useState<Set<string>>(new Set());
  const trend = useTrafficTrend(windowName);
  const dashboard = useDashboardTelemetry();
  const online = useOnlineUsers();

  const series = trend.data?.series?.logical_egresses || [];
  const visible = series.filter((item) => !hidden.has(item.id));
  const up = totalLatest(visible, "upload");
  const down = totalLatest(visible, "download");
  const onlineCount = online.data?.data?.items?.length || 0;
  const activeSessions = Number(dashboard.data?.data?.sessions ?? 0);
  const peaks = series.flatMap((item) => item.samples || []);
  const peak = peaks.length ? Math.max(...peaks.map((sample) => Number(sample.download_bps || 0) + Number(sample.upload_bps || 0))) : null;

  return (
    <div className="page">
      <PageHeader
        title="流量概况"
        subtitle="WAN 出口的上行、下行与采样趋势"
        actions={
          <>
            <div className="segmented">
              {WINDOWS.map((item) => (
                <button key={item.key} type="button" className={windowName === item.key ? "is-active" : ""} onClick={() => setWindowName(item.key)}>
                  {item.label}
                </button>
              ))}
            </div>
            <Button size="sm" icon={<RefreshCw size={14} />} onClick={() => trend.refetch()}>刷新</Button>
          </>
        }
      />

      <div className="metric-grid">
        <Metric label="当前下行" value={trend.isLoading ? "读取中" : formatRate(down)} hint="接收速率" tone="blue" icon={<ArrowDownRight size={15} />} />
        <Metric label="当前上行" value={trend.isLoading ? "读取中" : formatRate(up)} hint="发送速率" tone="violet" icon={<ArrowUpRight size={15} />} />
        <Metric label="活动连接" value={dashboard.isLoading ? "--" : activeSessions.toLocaleString("zh-CN")} hint="当前会话" tone="amber" />
        <Metric label="在线用户" value={online.isLoading ? "--" : onlineCount} hint="已分配地址" tone="green" />
        <Metric label="采样峰值" value={formatRate(peak)} hint={`更新时间 ${formatTime(trend.dataUpdatedAt ? new Date(trend.dataUpdatedAt).toISOString() : "")}`} tone="plain" />
      </div>

      <Card
        title="出口流量"
        subtitle={`${windowName === "24h" ? "最近 24 小时" : windowName === "1h" ? "最近 1 小时" : windowName === "5m" ? "最近 5 分钟" : "实时"} · 采样间隔 ${trend.data?.sampling_interval_seconds || "--"}s`}
        actions={<Badge tone={trend.data?.degraded ? "warning" : "success"} dot>{trend.data?.degraded ? "部分数据降级" : "数据正常"}</Badge>}
      >
        <div className="legend-row">
          {series.map((item, index) => (
            <button
              key={item.id}
              type="button"
              className={hidden.has(item.id) ? "legend-item is-hidden" : "legend-item"}
              onClick={() => setHidden((current) => {
                const next = new Set(current);
                if (next.has(item.id)) next.delete(item.id);
                else next.add(item.id);
                return next;
              })}
            >
              <i style={{ background: seriesColor(index) }} />
              <span>{item.name || item.id}</span>
              <em>{item.kind === "wan_group" ? "WAN 群组" : item.kind === "proxy" ? "代理" : "WAN"}</em>
            </button>
          ))}
        </div>
        {trend.data?.degraded && <div className="inline-warning">{sanitizeDetail(trend.data.degraded_reason, "部分流量数据暂不可用，图表可能不完整。")}</div>}
        <div className="traffic-pair">
          <section className="traffic-block">
            <header><div><h3>上行趋势</h3><span>出口发送流量</span></div><strong>{formatRate(up)}</strong></header>
            {trend.isLoading ? <Skeleton rows={4} /> : series.length ? <TrafficChart series={series} direction="upload" hidden={hidden} height={255} /> : <EmptyState title="暂无上行采样" description="暂无流量数据。" icon={<Activity size={22} />} />}
          </section>
          <section className="traffic-block">
            <header><div><h3>下行趋势</h3><span>出口接收流量</span></div><strong>{formatRate(down)}</strong></header>
            {trend.isLoading ? <Skeleton rows={4} /> : series.length ? <TrafficChart series={series} direction="download" hidden={hidden} height={255} /> : <EmptyState title="暂无下行采样" description="暂无流量数据。" icon={<Activity size={22} />} />}
          </section>
        </div>
      </Card>
    </div>
  );
}
