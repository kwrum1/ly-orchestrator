import { useNavigate } from "react-router-dom";
import {
  Activity,
  ArrowLeftRight,
  ArrowRight,
  Cable,
  ChartNoAxesCombined,
  GitBranch,
  Network,
  RefreshCcw,
  ServerCog,
  Settings2,
  ShieldCheck,
  Waypoints,
  Workflow,
} from "lucide-react";
import { useHealth, useRuntime, useTrafficTrend } from "../../api/hooks";
import { useConversions, useOrchestratorDashboard, useOrchestratorOperation, useOrchestratorPolicy, useOrchestratorTopology, useUpgrades } from "../../api/orchestrator-hooks";
import { Badge, Button, Card, EmptyState, Metric, PageHeader, ProgressBar } from "../../components/ui";
import { formatBytes, formatDateTime, stateLabel, text } from "../../lib/format";
import { sanitizeDetail } from "../../lib/human";

export function OrchestratorOverviewPage() {
  const navigate = useNavigate();
  const health = useHealth();
  const runtime = useRuntime();
  const topology = useOrchestratorTopology();
  const policy = useOrchestratorPolicy();
  const dashboard = useOrchestratorDashboard();
  const operation = useOrchestratorOperation();
  const upgrades = useUpgrades();
  const conversions = useConversions();
  const trend = useTrafficTrend("5m");

  const topo = topology.data?.item;
  const lan = topo?.interfaces?.find((item) => item.role === "lan");
  const wan = topo?.interfaces?.find((item) => item.role === "wan");
  const groups = topo?.orchestration_groups || [];
  const policyGroups = policy.data?.item?.policy_groups || [];
  const rules = policyGroups.reduce((sum, group) => sum + group.rules.length, 0);
  const components = runtime.data?.components || [];
  const degraded = components.filter((item) => item.available === false || item.state === "degraded");
  const op = operation.data?.operation || {};
  const version = upgrades.data?.version;
  const boundary = dashboard.data?.data?.boundary_totals || {};
  const latestTrend = trend.data?.series?.logical_egresses || [];

  return (
    <div className="page orch-overview">
      <PageHeader
        title="单机总览"
        subtitle="本机流量编排：拓扑、策略、透明编排与设备状态"
        actions={<><Button size="sm" icon={<RefreshCcw size={14} />} onClick={() => { topology.refetch(); policy.refetch(); runtime.refetch(); operation.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Settings2 size={14} />} onClick={() => navigate("/topology")}>编辑拓扑</Button></>}
        meta={<><span className="meta-item"><i className={`status-dot status-dot-${health.data?.status === "ok" ? "success" : "warning"}`} />{health.data?.status === "ok" ? "运行正常" : "存在降级"}</span><span className="meta-item">版本 {text(version?.version || health.data?.version, "--")}</span><span className="meta-item"><Network size={13} />{topo ? "拓扑已配置" : "拓扑未配置"}</span><span className="meta-item"><Workflow size={13} />{stateLabel(op.state || "idle")}</span></>}
      />

      <div className="metric-grid">
        <Metric label="逻辑 LAN" value={lan ? (lan.port || lan.bond?.members?.join(", ") || "聚合") : "未配置"} hint="本地 LAN 侧" tone="green" icon={<Cable size={15} />} />
        <Metric label="逻辑 WAN" value={wan ? (wan.port || wan.bond?.members?.join(", ") || "聚合") : "未配置"} hint="本地 WAN 侧" tone="blue" icon={<Cable size={15} />} />
        <Metric label="编排组" value={groups.length} hint="服务链路径" tone="violet" icon={<Waypoints size={15} />} />
        <Metric label="策略组 / 规则" value={`${policyGroups.length} / ${rules}`} hint="本地策略" tone="amber" icon={<GitBranch size={15} />} />
        <Metric label="当前操作" value={stateLabel(op.state || "idle")} hint={text(op.name, "无运行中操作")} tone="plain" icon={<Activity size={15} />} />
      </div>

      <div className="overview-main-grid">
        <Card className="span-8" title="本机拓扑" subtitle="管理口、逻辑 LAN/WAN 与编排组" actions={<Button size="sm" variant="text" onClick={() => navigate("/topology")}>编辑</Button>}>
          {topo ? (
            <div className="orch-overview-topology">
              <div className="orch-topo-node"><span className="orch-node-label">管理口</span><strong>{text(topo.management_interface)}</strong><small>{topo.management_shared ? "与 LAN 共享" : "独立管理口"}</small></div>
              <ArrowRight size={18} className="muted" />
              <div className="orch-topo-node is-lan"><span className="orch-node-label">LAN</span><strong>{lan?.port || lan?.bond?.members?.join(" ") || "未配置"}</strong><small>逻辑 LAN</small></div>
              <ArrowRight size={18} className="muted" />
              <div className="orch-topo-groups">
                {groups.map((group) => <div className="orch-topo-group" key={group.name}><Waypoints size={13} /><span>{group.name}</span><small>{group.ports.map((port) => `${port.direction}:${port.interface || port.bond?.name || "?"}`).join(" · ")}</small></div>)}
                {!groups.length && <span className="muted">无编排组</span>}
              </div>
              <ArrowRight size={18} className="muted" />
              <div className="orch-topo-node is-wan"><span className="orch-node-label">WAN</span><strong>{wan?.port || wan?.bond?.members?.join(" ") || "未配置"}</strong><small>逻辑 WAN</small></div>
            </div>
          ) : <EmptyState title="尚未配置拓扑" description="单机拓扑是本产品唯一拓扑；不会创建多站点。" action={<Button size="sm" variant="primary" onClick={() => navigate("/topology")}>配置拓扑</Button>} icon={<Network size={22} />} />}
          <div className="inline-warning"><ShieldCheck size={14} /> 单机本地编排，不是多站点/跨网关集中控制器；没有站点注册、心跳或集中下发。</div>
        </Card>

        <Card className="span-4" title="系统状态" subtitle="设备与最近应用" actions={<Button size="sm" variant="text" onClick={() => navigate("/runtime")}>查看</Button>}>
          <div className="health-metrics">
            <div className="health-metric"><span><ServerCog size={14} /> 组件</span><strong>{components.length}</strong><small className="muted">{degraded.length ? `${degraded.length} 个异常` : "全部可用"}</small></div>
            <div className="health-metric"><span><Activity size={14} /> 最近应用</span><strong>{stateLabel(runtime.data?.last_apply?.status || "unknown")}</strong><small className="muted">{formatDateTime(runtime.data?.last_apply?.applied_at)}</small></div>
            <div className="health-metric"><span><RefreshCcw size={14} /> 版本</span><strong>{text(health.data?.version, "--")}</strong><small className="muted">当前设备</small></div>
          </div>
        </Card>
      </div>

      <div className="overview-secondary-grid">
        <Card className="span-7" title="流量边界" subtitle="WAN/LAN 双向累计字节" actions={<Button size="sm" variant="text" onClick={() => navigate("/telemetry")}>流量监控</Button>}>
          <div className="orch-boundary">
            <div><span>WAN → LAN</span><strong>{formatBytes(boundary.wan?.wan_to_lan?.bytes)}</strong><small>{sanitizeDetail(boundary.wan?.wan_to_lan?.rate_state, "暂无速率")}</small></div>
            <div><span>LAN → WAN</span><strong>{formatBytes(boundary.wan?.lan_to_wan?.bytes)}</strong><small>{sanitizeDetail(boundary.wan?.lan_to_wan?.rate_state, "暂无速率")}</small></div>
            <div><span>LAN 边界</span><strong>{formatBytes(boundary.lan?.lan_to_wan?.bytes)}</strong><small>{sanitizeDetail(boundary.lan?.lan_to_wan?.rate_state, "暂无速率")}</small></div>
            <div><span>采样出口</span><strong>{latestTrend.length}</strong><small>个出口</small></div>
          </div>
          {dashboard.data?.data?.degraded && <div className="inline-warning">{sanitizeDetail(dashboard.data?.data?.degraded_reason, "部分流量数据暂不可用")}</div>}
        </Card>

        <Card className="span-5" title="策略与透明编排" subtitle="策略保存即同步透明编排" actions={<Button size="sm" variant="text" onClick={() => navigate("/policy")}>策略</Button>}>
          <dl className="fact-list">
            <div><dt>默认动作</dt><dd>{text(policy.data?.item?.default?.kind, "未配置")}</dd></div>
            <div><dt>策略组</dt><dd>{policyGroups.length}</dd></div>
            <div><dt>规则</dt><dd>{rules}</dd></div>
            <div><dt>IP 对象</dt><dd>{policy.data?.item?.ip_objects?.length || 0}</dd></div>
            <div><dt>当前操作</dt><dd>{text(op.name, "—")} · {stateLabel(op.state || "idle")}</dd></div>
          </dl>
          <div className="button-row">
            <Button size="sm" onClick={() => navigate("/transparent")} icon={<Workflow size={14} />}>透明编排</Button>
            <Button size="sm" onClick={() => navigate("/groups")} icon={<Waypoints size={14} />}>编排组</Button>
          </div>
        </Card>
      </div>

      <div className="overview-bottom-grid">
        <Card className="span-7" title="升级与转换" subtitle="独立版本；真实镜像转换暂不可用" actions={<Button size="sm" variant="text" onClick={() => navigate("/upgrades")}>升级</Button>}>
          <dl className="fact-list">
            <div><dt>当前版本</dt><dd>{text(version?.version, "--")} · {text(version?.suite, "--")}/{text(version?.arch, "--")}</dd></div>
            <div><dt>升级支持</dt><dd>{version?.upgrade_supported ? "支持" : "不支持"}</dd></div>
            <div><dt>升级任务</dt><dd>{upgrades.data?.items?.length || 0}</dd></div>
            <div><dt>转换任务</dt><dd>{conversions.data?.items?.length || 0}</dd></div>
            <div><dt>真实镜像转换</dt><dd><Badge tone="warning">暂不可用</Badge></dd></div>
          </dl>
          <div className="button-row"><Button size="sm" onClick={() => navigate("/upgrades")} icon={<RefreshCcw size={14} />}>升级 / 回滚</Button><Button size="sm" onClick={() => navigate("/conversions")} icon={<ArrowLeftRight size={14} />}>产品转换</Button></div>
        </Card>

        <Card className="span-5" title="边界与缺口" subtitle="不虚构后端能力">
          <ul className="boundary-list">
            <li>单机本地拓扑；无多站点/节点纳管。</li>
            <li>透明编排是 L3 服务链，不是 L2 桥接。</li>
            <li>转换仅支持校验、准备、回滚；真实镜像转换暂不可用。</li>
            <li>流量数据依赖数据面；不可用时显示提示。</li>
            <li>升级安装/回滚为异步任务，需要等待完成。</li>
          </ul>
          <Button size="sm" variant="secondary" onClick={() => navigate("/conversions")}>查看转换边界</Button>
        </Card>
      </div>
    </div>
  );
}
