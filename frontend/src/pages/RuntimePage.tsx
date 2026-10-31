import { useState } from "react";
import { Activity, Eye, Play, RefreshCw, ServerCog, ShieldAlert } from "lucide-react";
import { API } from "../api/endpoints";
import { getJson } from "../api/client";
import { useApplyState, useRuntime } from "../api/hooks";
import type { RuntimeApply, RuntimePreview } from "../api/types";
import { Badge, Button, Card, EmptyState, ErrorState, PageHeader, Skeleton, Tabs } from "../components/ui";
import { applyOutcomeLabel, applyOutcomeOf, useApply, useAuth } from "../state/providers";
import { formatDateTime, stateLabel } from "../lib/format";
import { applyFailureMessage, sanitizeDetail } from "../lib/human";
import { NO_PROXY_EGRESS } from "../lib/proxy";

const IDLE_COMPONENT_STATES = new Set(["not_configured", "disabled", "unconfigured", ""]);

function truncateText(value: string, limit = 160): string {
  const text = String(value || "").trim();
  return text.length > limit ? `${text.slice(0, limit)}…` : text;
}

function count(value: unknown) {
  return Array.isArray(value) ? value.length : 0;
}

function outcomeTone(outcome: string): "success" | "warning" | "danger" | "info" | "neutral" {
  if (outcome === "applied") return "success";
  if (outcome === "partial") return "warning";
  if (outcome === "failed") return "danger";
  if (outcome === "unchanged" || outcome === "skipped") return "info";
  return "neutral";
}

function generationValue(...values: unknown[]): string {
  for (const value of values) {
    if (typeof value === "number" && Number.isFinite(value)) return String(value);
    if (typeof value === "string" && value.trim()) return value;
  }
  return "--";
}

export function RuntimePage() {
  const runtime = useRuntime();
  const applyState = useApplyState();
  const apply = useApply();
  const { session } = useAuth();
  const [preview, setPreview] = useState<RuntimePreview | null>(null);
  const [previewBusy, setPreviewBusy] = useState(false);
  const [previewError, setPreviewError] = useState<unknown>(null);
  const [tab, setTab] = useState("overview");

  const components = runtime.data?.components || [];
  const localResult = apply.result;
  const last: RuntimeApply | undefined = runtime.data?.last_apply || localResult || undefined;
  const degraded = components.filter((item) => item.available === false || item.state === "degraded");
  const plan = preview?.plan || {};
  const state = applyState.data;
  const stateEnabled = state?.enabled !== false && state?.status !== "unconfigured";
  const outcome = applyOutcomeOf(localResult) || applyOutcomeOf(last);
  const desired = generationValue(state?.desired_generation, state?.desired?.generation, last?.desired_generation);
  const applied = generationValue(state?.applied_generation, state?.applied?.generation, last?.applied_generation);
  const current = generationValue(state?.current_generation, state?.current?.generation, last?.current_generation);
  const degradedReasons = state?.degraded_reasons || last?.degraded_reasons || [];
  const lastError = state?.last_error || last?.last_error;
  const partial = Boolean(state?.partial || last?.partial || outcome === "partial");
  const stateFailed = state?.status === "failed" || outcome === "failed";
  const unchanged = state?.status === "unchanged" || outcome === "unchanged" || outcome === "skipped";
  const notConverged = stateEnabled && desired !== "--" && applied !== "--" && desired !== applied;

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
        subtitle="组件健康、应用事务、最近应用与配置预览"
        actions={<><Button size="sm" icon={<Eye size={14} />} loading={previewBusy} onClick={loadPreview}>预览</Button><Button size="sm" variant="primary" icon={<Play size={14} />} loading={apply.running} onClick={() => void apply.apply()} disabled={session?.role === "readonly"}>应用配置</Button></>}
      />

      <div className="status-strip">
        <span>总状态 <Badge tone={runtime.data?.status === "running" ? "success" : degraded.length ? "warning" : "neutral"} dot>{stateLabel(runtime.data?.status || "unknown")}</Badge></span>
        <span>组件 <b>{components.length}</b></span>
        <span>异常 <b>{degraded.length}</b></span>
      </div>

      <Card
        title="应用事务"
        subtitle="期望、已应用与当前数据面 generation"
        actions={<Button size="sm" variant="ghost" icon={<RefreshCw size={13} />} loading={applyState.isFetching} onClick={() => applyState.refetch()}>刷新</Button>}
      >
        {applyState.isLoading ? <Skeleton rows={4} /> : applyState.isError ? <ErrorState error={applyState.error} onRetry={() => applyState.refetch()} /> : !stateEnabled ? (
          <EmptyState
            title="事务状态未启用"
            description="设备没有可用的 apply 状态存储；仍可应用配置，但无法比较 generation。"
            icon={<Activity size={22} />}
          />
        ) : (
          <div className="form-stack">
            <div className="plan-summary">
              <div><span>期望 generation</span><strong>{desired}</strong></div>
              <div><span>已应用 generation</span><strong>{applied}</strong></div>
              <div><span>当前数据面 generation</span><strong>{current}</strong></div>
              <div><span>状态</span><strong>{stateLabel(state?.status)}</strong></div>
              <div><span>回执</span><strong>{state?.receipt_status || "--"}</strong></div>
              <div><span>更新时间</span><strong className="mono">{formatDateTime(state?.updated_at)}</strong></div>
            </div>
            <div className="dependency-section-head">
              <div className="button-row">
                <Badge tone={stateFailed ? "danger" : partial ? "warning" : "success"} dot>{stateFailed ? "失败" : partial ? "部分成功" : stateLabel(state?.status)}</Badge>
                {notConverged && <Badge tone="warning">尚未收敛：已应用 generation 落后于期望</Badge>}
                {unchanged && <Badge tone="info">未变更，未重新下发</Badge>}
              </div>
            </div>
            {degradedReasons.length > 0 && (
              <div className="inline-warning">
                <ShieldAlert size={15} />
                <div>
                  <strong>降级原因（{degradedReasons.length} 项）</strong>
                  <ul className="warning-list">{degradedReasons.map((reason, index) => <li key={index}>{sanitizeDetail(reason, String(reason))}</li>)}</ul>
                </div>
              </div>
            )}
            {lastError && (
              <div className="error-state">
                <ShieldAlert size={18} />
                <div>
                  <strong>最近一次失败</strong>
                  <p>{sanitizeDetail(lastError.reason, "应用失败，请查看设备日志")}</p>
                  <p className="muted">
                    回执：{lastError.receipt_status || "--"}
                    {" · "}
                    时间：{formatDateTime(lastError.at)}
                  </p>
                </div>
              </div>
            )}
          </div>
        )}
      </Card>

      <div className="runtime-layout">
        <Card title="组件健康" subtitle="当前服务状态" actions={<Badge tone={degraded.length ? "warning" : "success"} dot>{degraded.length ? `${degraded.length} 个异常` : "全部正常"}</Badge>}>
          {runtime.isLoading ? <Skeleton rows={5} /> : runtime.isError ? <ErrorState error={runtime.error} onRetry={() => runtime.refetch()} /> : (
            <div className="component-grid">
              {components.map((component) => {
                const name = String(component.name || "").toLowerCase();
                const proxy = /xray|proxy|代理/.test(name);
                const state = String(component.state || "").toLowerCase();
                const unavailable = component.available === false;
                const idleProxy = proxy && IDLE_COMPONENT_STATES.has(state);
                const componentReason = sanitizeDetail(component.reason, "");
                const lastApplyReason = truncateText(sanitizeDetail(lastError?.reason, ""));
                return (
                  <div className="component-card" key={component.name}>
                    <div className="component-card-head"><ServerCog size={15} /><strong>{component.name}</strong><Badge tone={idleProxy ? "neutral" : unavailable ? "warning" : "success"} dot>{idleProxy ? NO_PROXY_EGRESS : stateLabel(component.state)}</Badge></div>
                    {idleProxy ? (
                      <p className="muted">{NO_PROXY_EGRESS}</p>
                    ) : unavailable ? (
                      <>
                        <p>{componentReason || "该组件当前不可用"}</p>
                        {lastApplyReason && <p className="muted">上次 apply 失败：{lastApplyReason}</p>}
                        <p className="muted">建议：查看“最近一次失败”的完整原因，修复后重新应用配置。</p>
                      </>
                    ) : (
                      <p className="muted">运行正常</p>
                    )}
                  </div>
                );
              })}
              {!components.length && <EmptyState title="暂无组件" description="暂无组件信息。" icon={<Activity size={22} />} />}
            </div>
          )}
        </Card>

        <Card
          title="最近应用"
          subtitle="结果以 outcome 与 generation 为准，不以“已提交”代替重新下发"
          actions={outcome ? <Badge tone={outcomeTone(outcome)}>{applyOutcomeLabel(outcome)}</Badge> : last?.status ? <Badge tone={String(last.status).includes("fail") ? "danger" : last.runtime_state === "degraded" ? "warning" : "success"}>{stateLabel(last.status)}</Badge> : null}
        >
          {last ? (
            <div className="apply-detail">
              <dl>
                <div><dt>结果</dt><dd>{applyOutcomeLabel(outcome)}</dd></div>
                <div><dt>状态</dt><dd>{stateLabel(last.runtime_state || last.status)}</dd></div>
                <div><dt>完成时间</dt><dd>{formatDateTime(last.applied_at)}</dd></div>
                <div><dt>应用服务</dt><dd>{(last.applied_services || []).join(", ") || "无"}</dd></div>
                <div><dt>Generation</dt><dd className="mono">期望 {generationValue(last.desired_generation, desired)} · 已应用 {generationValue(last.applied_generation, applied)}</dd></div>
                <div><dt>部分成功</dt><dd>{last.partial || outcome === "partial" ? "是" : "否"}</dd></div>
                <div><dt>说明</dt><dd>{describeApply(last, outcome)}</dd></div>
              </dl>
              {(last.degraded_reasons || []).length > 0 && (
                <ul className="warning-list">
                  {(last.degraded_reasons || []).map((reason, index) => <li key={index}>{sanitizeDetail(reason, String(reason))}</li>)}
                </ul>
              )}
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

function describeApply(result: RuntimeApply, outcome: string): string {
  if (outcome === "failed") return applyFailureMessage(result);
  if (outcome === "unchanged" || outcome === "skipped") return "配置未变更，本次没有重新下发；generation 保持不变。";
  if (outcome === "partial") {
    const reasons = (result.degraded_reasons || []).filter(Boolean);
    return reasons.length ? `部分成功：${reasons.map((reason) => sanitizeDetail(reason, String(reason))).join("；")}` : "部分成功：事务已提交，但存在降级步骤。";
  }
  if (outcome === "applied") return "配置已应用并收敛。";
  if (result.status === "skipped") return "配置未变化，已跳过。";
  if (!outcome && String(result.status || "").toLowerCase() === "committed") {
    return "状态为已提交，但未提供 outcome；无法据此判断本次是否重新下发。";
  }
  return "配置已应用。";
}
