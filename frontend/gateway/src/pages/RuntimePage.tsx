import { useState } from "react";
import { Activity, Eye, Play, RefreshCw, ServerCog, ShieldAlert } from "lucide-react";
import { API } from "../api/endpoints";
import { getJson } from "../api/client";
import { useRuntime } from "../api/hooks";
import type { RuntimePreview } from "../api/types";
import { Badge, Button, Card, EmptyState, ErrorState, PageHeader, Skeleton, Tabs } from "../components/ui";
import { useApply, useAuth } from "../state/providers";
import { formatDateTime, stateLabel } from "../lib/format";
import { applyFailureMessage, sanitizeDetail } from "../lib/human";

function count(value: unknown) {
  return Array.isArray(value) ? value.length : 0;
}

export function RuntimePage() {
  const runtime = useRuntime();
  const apply = useApply();
  const { session } = useAuth();
  const [preview, setPreview] = useState<RuntimePreview | null>(null);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState<unknown>(null);
  const [tab, setTab] = useState("overview");

  const components = runtime.data?.components || [];
  const last = runtime.data?.last_apply || apply.result;
  const degraded = components.filter((item) => item.available === false || item.state === "degraded");
  const plan = preview?.plan || {};

  const loadPreview = async () => {
    setPreviewBusy(true);
    setPreviewError(null);
    try {
      const result = await getJson<RuntimePreview>(API.runtime.preview, 30_000);
      setPreview(result);
    } catch (error) {
      setPreviewError(error);
    } finally {
      setPreviewBusy(false);
    }
  };

  return (
    <div className="page">
      <PageHeader
        title="系统状态"
        subtitle="组件健康、最近应用与配置预览"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => runtime.refetch()}>刷新</Button><Button size="sm" icon={<Eye size={14} />} loading={previewBusy} onClick={loadPreview}>预览</Button><Button size="sm" variant="primary" icon={<Play size={14} />} loading={apply.running} onClick={() => void apply.apply()} disabled={session?.role === "readonly"}>应用配置</Button></>}
      />

      <div className="status-strip">
        <span>总状态 <Badge tone={runtime.data?.status === "running" ? "success" : degraded.length ? "warning" : "neutral"} dot>{stateLabel(runtime.data?.status || "unknown")}</Badge></span>
        <span>组件 <b>{components.length}</b></span>
        <span>异常 <b>{degraded.length}</b></span>
      </div>

      <div className="runtime-layout">
        <Card title="组件健康" subtitle="当前服务状态" actions={<Badge tone={degraded.length ? "warning" : "success"} dot>{degraded.length ? `${degraded.length} 个异常` : "全部正常"}</Badge>}>
          {runtime.isLoading ? <Skeleton rows={5} /> : runtime.isError ? <ErrorState error={runtime.error} onRetry={() => runtime.refetch()} /> : (
            <div className="component-grid">
              {components.map((component) => (
                <div className="component-card" key={component.name}>
                  <div className="component-card-head"><ServerCog size={15} /><strong>{component.name}</strong><Badge tone={component.available === false ? "warning" : "success"} dot>{stateLabel(component.state)}</Badge></div>
                  <p className={component.available === false ? "" : "muted"}>{component.available === false ? "该组件当前不可用" : "运行正常"}</p>
                </div>
              ))}
              {!components.length && <EmptyState title="暂无组件" description="暂无组件信息。" icon={<Activity size={22} />} />}
            </div>
          )}
        </Card>

        <Card title="最近应用" subtitle="服务、时间与结果" actions={last?.status ? <Badge tone={String(last.status).includes("fail") ? "danger" : last.runtime_state === "degraded" ? "warning" : "success"}>{stateLabel(last.status)}</Badge> : null}>
          {last ? (
            <div className="apply-detail">
              <dl>
                <div><dt>状态</dt><dd>{stateLabel(last.runtime_state || last.status)}</dd></div>
                <div><dt>完成时间</dt><dd>{formatDateTime(last.applied_at)}</dd></div>
                <div><dt>应用服务</dt><dd>{(last.applied_services || []).join(", ") || "无"}</dd></div>
                <div><dt>说明</dt><dd>{last.status && String(last.status).includes("fail") ? applyFailureMessage(last) : "配置已应用"}</dd></div>
              </dl>
              {last.rollback && <div className="inline-warning"><ShieldAlert size={14} /> 本次应用已执行回滚，请检查设备状态。</div>}
            </div>
          ) : <EmptyState title="尚未应用" description="执行一次应用后，结果显示在这里。" icon={<Play size={22} />} />}
        </Card>
      </div>

      <Card title="配置预览" subtitle="只读计划，不会修改设备">
        {previewError ? <ErrorState error={previewError} onRetry={loadPreview} /> : !preview ? (
          <EmptyState title="尚未预览" description="点击“预览”查看 DNS、服务制品、数据面操作与警告。" action={<Button size="sm" onClick={loadPreview} loading={previewBusy}>预览配置</Button>} icon={<Eye size={22} />} />
        ) : (
          <>
            <div className="plan-summary">
              <div><span>DNS 策略</span><strong>{count(plan.dns_policies)}</strong></div>
              <div><span>服务制品</span><strong>{count(plan.service_artifacts)}</strong></div>
              <div><span>数据面操作</span><strong>{count(plan.vpp_operations)}</strong></div>
              <div><span>DHCP 服务</span><strong>{count(plan.dhcp_servers)}</strong></div>
              <div><span>PPPoE 对端</span><strong>{count(plan.pppoe_peers)}</strong></div>
              <div><span>转发表</span><strong>{plan.nftables_tproxy_plan ? "已编译" : "未返回"}</strong></div>
            </div>
            <Tabs value={tab} onChange={setTab} items={[
              { key: "overview", label: "概览" },
              { key: "artifacts", label: "服务制品", count: count(plan.service_artifacts) },
              { key: "vpp", label: "数据面操作", count: count(plan.vpp_operations) },
              { key: "warnings", label: "警告", count: count(plan.warnings) },
            ]} />
            {tab === "overview" && (
              <ul className="fact-list">
                <li>DNS 策略 {count(plan.dns_policies)} 条，DHCP 服务 {count(plan.dhcp_servers)} 个。</li>
                <li>PPPoE 对端 {count(plan.pppoe_peers)} 个，数据面操作 {count(plan.vpp_operations)} 项。</li>
                <li>{plan.nftables_tproxy_plan ? "转发表已编译。" : "未返回转发表计划。"}</li>
              </ul>
            )}
            {tab === "artifacts" && <div className="mini-table-wrap"><table className="data-table"><thead><tr><th>服务</th><th>路径</th><th>重载方式</th></tr></thead><tbody>{(plan.service_artifacts || []).map((item: any, index: number) => <tr key={index}><td>{String(item.service || "--")}</td><td className="mono">{String(item.path || "--")}</td><td>{String(item.reload_mode || "--")}</td></tr>)}</tbody></table></div>}
            {tab === "vpp" && <div className="mini-table-wrap"><table className="data-table"><thead><tr><th>名称</th><th>资源</th></tr></thead><tbody>{(plan.vpp_operations || []).map((item: any, index: number) => <tr key={index}><td>{String(item.Name || item.name || "--")}</td><td>{String(item.Resource || item.resource || "--")}</td></tr>)}</tbody></table></div>}
            {tab === "warnings" && (plan.warnings?.length ? <ul className="warning-list">{plan.warnings.map((warning: any, index: number) => <li key={index}>{sanitizeDetail(warning, "存在一条未命名提示")}</li>)}</ul> : <EmptyState title="无警告" description="预览未返回警告。" icon={<Activity size={22} />} />)}
          </>
        )}
      </Card>
    </div>
  );
}
