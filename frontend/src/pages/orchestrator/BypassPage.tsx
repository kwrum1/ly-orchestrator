import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { AlertTriangle, RefreshCw, Save, ShieldAlert } from "lucide-react";
import { API } from "../../api/endpoints";
import { postJson, putJson } from "../../api/client";
import { orchKeys, useOrchestratorBypass } from "../../api/orchestrator-hooks";
import type { BypassConfig, BypassFlowTarget, BypassGroupState, BypassState, BypassTCPTarget } from "../../api/types";
import { Badge, Button, Card, EmptyState, Field, Input, PageHeader, Skeleton, Switch, Textarea, useToast } from "../../components/ui";
import { useAuth } from "../../state/providers";

// 故障旁路 (per-group fault bypass). The backend detector watches each
// orchestration group independently and flips only that group's plugin flag.
// A group with no traffic is `unknown` and never bypasses; only a full window
// with attempts > 0 and zero success is a fault. A manual force suspends
// automatic recovery until the operator clears it.

type GroupEdit = {
  window_seconds: string;
  failure_threshold: string;
  recovery_threshold: string;
  interfaces: string;
  flow_targets: string;
  tcp_targets: string;
};

type GlobalEdit = {
  enabled: boolean;
  interval_seconds: string;
  timeout_ms: string;
  failure_threshold: string;
  recovery_threshold: string;
};

const HEALTH_LABELS: Record<string, string> = {
  unknown: "无流量 / 未知",
  healthy: "正常",
  faulty: "故障",
  bypass: "旁路中",
};

function healthTone(health?: string): "neutral" | "success" | "warning" | "danger" {
  switch (health) {
    case "healthy": return "success";
    case "faulty": return "warning";
    case "bypass": return "danger";
    default: return "neutral";
  }
}

function flowTargetsText(targets?: BypassFlowTarget[]) {
  return (targets || []).map((target) => `${target.dst_ip}:${target.dst_port}/${target.protocol || "tcp"}`).join("\n");
}

function tcpTargetsText(targets?: BypassTCPTarget[]) {
  return (targets || []).map((target) => target.source ? `${target.target} @ ${target.source}` : target.target).join("\n");
}

function parseFlowTargets(text: string): BypassFlowTarget[] {
  const result: BypassFlowTarget[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const match = line.match(/^([0-9a-fA-F:.]+):(\d{1,5})(?:\/(tcp|udp))?$/);
    if (!match) throw new Error(`无法解析被动探测目标：${line}（格式 ip:port/tcp 或 ip:port/udp）`);
    const port = Number(match[2]);
    if (!Number.isInteger(port) || port < 1 || port > 65535) throw new Error(`端口超出范围：${line}`);
    result.push({ dst_ip: match[1], dst_port: port, protocol: match[3] || "tcp" });
  }
  return result;
}

function parseTCPTargets(text: string): BypassTCPTarget[] {
  const result: BypassTCPTarget[] = [];
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const [target, source] = line.split("@").map((value) => value.trim());
    if (!/^[^\s]+:\d{1,5}$/.test(target)) throw new Error(`无法解析主动探测目标：${line}（格式 host:port 或 host:port @ source）`);
    result.push(source ? { target, source } : { target });
  }
  return result;
}

function numberValue(value: string, fallback: number, label: string, min = 1) {
  const parsed = Number(value);
  if (!Number.isFinite(parsed) || parsed < min) throw new Error(`${label} 必须是 ≥ ${min} 的数字`);
  return parsed;
}

function editFromConfig(config?: BypassConfig): GlobalEdit {
  return {
    enabled: Boolean(config?.enabled),
    interval_seconds: String(config?.interval_seconds || 2),
    timeout_ms: String(config?.timeout_ms || 1000),
    failure_threshold: String(config?.failure_threshold || 3),
    recovery_threshold: String(config?.recovery_threshold || 3),
  };
}

function groupEditFromConfig(config?: BypassConfig, group?: BypassGroupState): GroupEdit {
  const stored = (config?.groups || []).find((item) => item.name === group?.group);
  return {
    window_seconds: String(stored?.window_seconds || group?.window_seconds || 10),
    failure_threshold: String(stored?.failure_threshold || ""),
    recovery_threshold: String(stored?.recovery_threshold || ""),
    interfaces: (stored?.interfaces || group?.interfaces || []).join(", "),
    flow_targets: flowTargetsText(stored?.flow_targets || group?.flow_targets),
    tcp_targets: tcpTargetsText(stored?.tcp_targets || group?.tcp_targets),
  };
}

export function OrchestratorBypassPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const { session } = useAuth();
  const bypass = useOrchestratorBypass();
  const readonly = session?.role === "readonly";
  const state = bypass.data;
  const groups = useMemo(() => state?.groups || [], [state]);
  const [global, setGlobal] = useState<GlobalEdit | null>(null);
  const [edits, setEdits] = useState<Record<string, GroupEdit> | null>(null);
  const [busy, setBusy] = useState<string>("");
  const [error, setError] = useState("");

  // Seed the editor once from the first GET. Polling must not overwrite what
  // the operator is typing; after a save we clear the draft and re-seed from
  // the response.
  useEffect(() => {
    if (!state || global || edits) return;
    setGlobal(editFromConfig(state.config));
    const seeded: Record<string, GroupEdit> = {};
    for (const group of state.groups || []) seeded[group.group] = groupEditFromConfig(state.config, group);
    setEdits(seeded);
  }, [state, global, edits]);

  const reload = async () => {
    setGlobal(null);
    setEdits(null);
    await bypass.refetch();
  };

  const applyState = (next: BypassState) => {
    queryClient.setQueryData(orchKeys.bypass, next);
    setGlobal(null);
    setEdits(null);
  };

  const toggle = async (group: string, enabled: boolean) => {
    const label = group || "全局";
    setBusy(`toggle:${label}`);
    setError("");
    try {
      const next = await postJson<BypassState>(API.orchestrator.bypass, {
        ...(group ? { group } : {}),
        enabled,
        reason: "ui manual",
      });
      applyState(next);
      toast(enabled ? `${label} 已强制旁路` : `${label} 已恢复策略`, "success");
    } catch (failed: any) {
      const message = failed?.message || "切换旁路失败";
      setError(message);
      toast(message, "error");
    } finally {
      setBusy("");
    }
  };

  const saveConfig = async () => {
    if (!state || !global || !edits) return;
    setBusy("save");
    setError("");
    try {
      const config: BypassConfig = {
        ...state.config,
        enabled: global.enabled,
        interval_seconds: numberValue(global.interval_seconds, 2, "探测间隔", 1),
        timeout_ms: numberValue(global.timeout_ms, 1000, "探测超时", 100),
        failure_threshold: numberValue(global.failure_threshold, 3, "失败阈值", 1),
        recovery_threshold: numberValue(global.recovery_threshold, 3, "恢复阈值", 1),
        groups: groups.map((group) => {
          const edit = edits[group.group] || groupEditFromConfig(state.config, group);
          const raw = {
            name: group.group,
            window_seconds: numberValue(edit.window_seconds, 10, "窗口", 1),
            interfaces: edit.interfaces.split(",").map((value) => value.trim()).filter(Boolean),
            flow_targets: parseFlowTargets(edit.flow_targets),
            tcp_targets: parseTCPTargets(edit.tcp_targets),
          };
          const failure = edit.failure_threshold.trim() ? numberValue(edit.failure_threshold, 3, "失败阈值", 1) : 0;
          const recovery = edit.recovery_threshold.trim() ? numberValue(edit.recovery_threshold, 3, "恢复阈值", 1) : 0;
          return {
            ...raw,
            ...(failure ? { failure_threshold: failure } : {}),
            ...(recovery ? { recovery_threshold: recovery } : {}),
          };
        }),
      };
      const next = await putJson<BypassState>(API.orchestrator.bypassConfig, config, 30_000);
      applyState(next);
      toast("故障旁路配置已保存", "success");
    } catch (failed: any) {
      const message = failed?.message || "保存故障旁路配置失败";
      setError(message);
      toast(message, "error");
    } finally {
      setBusy("");
    }
  };

  if (bypass.isLoading || !state || !global || !edits) {
    if (bypass.isError) {
      return (
        <div className="page">
          <Card title="故障旁路读取失败">
            <div className="error-state">{String((bypass.error as any)?.message || bypass.error)}</div>
          </Card>
        </div>
      );
    }
    return <div className="page"><Skeleton rows={8} /></div>;
  }

  return (
    <div className="page">
      <PageHeader
        title="故障旁路"
        subtitle="按编排组独立检测：请求发出但满窗口零响应时，仅旁路该组的策略"
        actions={
          <>
            <Button size="sm" onClick={() => { void reload(); }} loading={bypass.isFetching}>
              <RefreshCw size={14} />刷新
            </Button>
            <Button size="sm" variant="primary" loading={busy === "save"} onClick={saveConfig} disabled={readonly}>
              <Save size={14} />保存配置
            </Button>
          </>
        }
        meta={
          <>
            <span className="meta-item"><Badge tone={state.enabled ? "danger" : "success"} dot>{state.enabled ? "有组处于旁路" : "策略强制中"}</Badge></span>
            <span className="meta-item">策略代 {state.generation || "未知"}</span>
            <span className="meta-item">旁路包数 {state.bypass_packets || 0}</span>
          </>
        }
      />

      {error && <div className="error-state"><strong>操作失败</strong><p>{error}</p></div>}

      <Card title="判定语义" subtitle="避免把“没有业务流量”误判成故障">
        <div className="notice-block">
          <strong>无流量 = unknown，不旁路；满窗口有请求且零成功才旁路</strong>
          <span>
            数据面按组统计发往探测目标的请求与反向响应。窗口内请求数（attempts）为 0 时健康状态保持 unknown，
            绝不切换路径；只有 attempts &gt; 0 且 success = 0 且窗口已满时才进入 bypass。
            收到任意响应（含 DNS 错误应答或应用层错误页）都算成功；RST 与 ICMP 不可达计为失败证据。
            手工强制旁路会暂停自动恢复，直到再次手工恢复。
          </span>
        </div>
      </Card>

      <Card
        title="全局探测"
        subtitle="关闭后不再自动进入旁路，手工强制仍然有效"
        actions={<Badge tone={global.enabled ? "info" : "neutral"}>{global.enabled ? "自动探测开启" : "自动探测关闭"}</Badge>}
      >
        <div className="form-grid">
          <Field label="自动故障检测">
            <Switch checked={global.enabled} disabled={readonly} onChange={(checked) => setGlobal({ ...global, enabled: checked })} label={global.enabled ? "开启" : "关闭"} />
          </Field>
          <Field label="探测间隔（秒）"><Input value={global.interval_seconds} disabled={readonly} onChange={(event) => setGlobal({ ...global, interval_seconds: event.target.value })} /></Field>
          <Field label="探测超时（毫秒）"><Input value={global.timeout_ms} disabled={readonly} onChange={(event) => setGlobal({ ...global, timeout_ms: event.target.value })} /></Field>
          <Field label="连续失败轮数"><Input value={global.failure_threshold} disabled={readonly} onChange={(event) => setGlobal({ ...global, failure_threshold: event.target.value })} /></Field>
          <Field label="连续恢复轮数"><Input value={global.recovery_threshold} disabled={readonly} onChange={(event) => setGlobal({ ...global, recovery_threshold: event.target.value })} /></Field>
          <Field label="全局手工旁路">
            <Button
              size="sm"
              variant={state.forced ? "primary" : "danger"}
              loading={busy === "toggle:全局"}
              disabled={readonly}
              onClick={() => { void toggle("", !state.forced); }}
            >
              <ShieldAlert size={14} />{state.forced ? "恢复全局策略" : "强制全局旁路"}
            </Button>
          </Field>
        </div>
      </Card>

      {!groups.length && (
        <Card title="编排组">
          <EmptyState title="拓扑中还没有编排组" description="先保存带编排组的拓扑，故障旁路会为每个组分别检测。" />
        </Card>
      )}

      {groups.map((group) => {
        const edit = edits[group.group] || groupEditFromConfig(state.config, group);
        const update = (patch: Partial<GroupEdit>) => setEdits({ ...edits, [group.group]: { ...edit, ...patch } });
        return (
          <Card
            key={group.group}
            title={<span className="inline-flex"><ShieldAlert size={16} /> {group.group}</span>}
            subtitle={`窗口 ${group.window_seconds || edit.window_seconds}s · 请求 ${group.flow_attempts || 0} · 成功 ${group.flow_success || 0} · RST ${group.flow_rst || 0} · 超时 ${group.flow_timeouts || 0}`}
            actions={
              <>
                <Badge tone={healthTone(group.health)} dot>{HEALTH_LABELS[group.health] || group.health}</Badge>
                {group.enabled && <Badge tone="danger">旁路中</Badge>}
                {group.forced && <Badge tone="warning">手工强制</Badge>}
                <Button
                  size="sm"
                  variant={group.enabled ? "primary" : "danger"}
                  loading={busy === `toggle:${group.group}`}
                  disabled={readonly}
                  onClick={() => { void toggle(group.group, !group.enabled); }}
                >
                  {group.enabled ? "恢复该组策略" : "强制该组旁路"}
                </Button>
              </>
            }
          >
            <div className="form-stack">
              {(group.reason || group.cause) && (
                <div className={group.enabled ? "inline-warning" : "notice-block"}>
                  <strong>{group.enabled ? "当前旁路原因" : "最近状态"}</strong>
                  <span>{group.reason || group.cause}{group.since ? ` · 自 ${group.since}` : ""}</span>
                </div>
              )}
              <div className="form-grid">
                <Field label="探测窗口（秒）"><Input value={edit.window_seconds} disabled={readonly} onChange={(event) => update({ window_seconds: event.target.value })} /></Field>
                <Field label="失败阈值（留空用全局）"><Input value={edit.failure_threshold} placeholder={global.failure_threshold} disabled={readonly} onChange={(event) => update({ failure_threshold: event.target.value })} /></Field>
                <Field label="恢复阈值（留空用全局）"><Input value={edit.recovery_threshold} placeholder={global.recovery_threshold} disabled={readonly} onChange={(event) => update({ recovery_threshold: event.target.value })} /></Field>
                <Field label="探测网口（可留空，默认取组端口）">
                  <Input value={edit.interfaces} placeholder="ens160, ens193" disabled={readonly} onChange={(event) => update({ interfaces: event.target.value })} />
                </Field>
              </div>
              <Field label="被动探测目标" hint="每行一个 ip:port/protocol；仅用于请求/响应型业务（网页、数据库、DNS、NTP、QUIC）">
                <Textarea rows={3} value={edit.flow_targets} placeholder={"192.168.10.82:80/tcp\n192.168.10.82:53/udp"} disabled={readonly} onChange={(event) => update({ flow_targets: event.target.value })} />
              </Field>
              <Field label="主动 TCP 探测（可选）" hint="每行 host:port，或 host:port @ source；源地址不可用时探测为“不确定”，不会误旁路">
                <Textarea rows={2} value={edit.tcp_targets} placeholder="10.1.18.10:443" disabled={readonly} onChange={(event) => update({ tcp_targets: event.target.value })} />
              </Field>
              {(group.flow_stats || []).length > 0 && (
                <div className="notice-block">
                  <strong>插件累计计数</strong>
                  <span>
                    {(group.flow_stats || []).map((stat) => (
                      `${stat.dst_ip}:${stat.dst_port}/${stat.protocol} attempts ${stat.attempts} success ${stat.success} rst ${stat.rst || 0} icmp ${stat.icmp_unreachable || 0}`
                    )).join("；")}
                  </span>
                </div>
              )}
              {(group.probes || []).length > 0 && (
                <div className="notice-block">
                  <strong>最近探测</strong>
                  <span>{(group.probes || []).slice(0, 4).map((probe) => `${probe.kind}:${probe.target} ${probe.ok ? "OK" : "FAIL"}${probe.detail ? ` (${probe.detail})` : ""}`).join("；")}</span>
                </div>
              )}
            </div>
          </Card>
        );
      })}

      <div className="inline-warning">
        <AlertTriangle size={14} /> 旁路只跳过该组的策略评估，不改动已提交的策略代；恢复后立即重新执行原策略。物理路径本身中断时，旁路不能修复链路。
      </div>
    </div>
  );
}
