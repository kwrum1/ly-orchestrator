import { useState } from "react";
import { Activity, ArrowRight, Play, RefreshCw, Workflow, Zap } from "lucide-react";
import { API } from "../../api/endpoints";
import { postJson } from "../../api/client";
import { useOrchestratorOperation } from "../../api/orchestrator-hooks";
import { useOrchestratorTopology } from "../../api/orchestrator-hooks";
import { Badge, Button, Card, Field, Input, PageHeader, Select, useToast } from "../../components/ui";
import { useAuth } from "../../state/providers";
import { formatDateTime, stateLabel, text } from "../../lib/format";

export function OrchestratorTransparentPage() {
  const { toast } = useToast();
  const { session } = useAuth();
  const topology = useOrchestratorTopology();
  const operation = useOrchestratorOperation();
  const [flow, setFlow] = useState<any>({ protocol: "tcp", source_port: 12345, destination_port: 443 });
  const [bindings, setBindings] = useState<any[]>([{ group: "", wan_facing_next_hop: "", lan_facing_next_hop: "" }]);
  const [result, setResult] = useState<any>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");

  const groups = topology.data?.item?.orchestration_groups || [];
  const op = operation.data?.operation || {};

  const body = () => ({
    flow: {
      source_ip: flow.source_ip,
      destination_ip: flow.destination_ip,
      protocol: flow.protocol,
      source_port: Number(flow.source_port) || 0,
      destination_port: Number(flow.destination_port) || 0,
    },
    prelude: {},
    bindings: bindings.filter((item) => item.group).map((item) => ({ group: item.group, wan_facing_next_hop: item.wan_facing_next_hop, lan_facing_next_hop: item.lan_facing_next_hop })),
  });

  const compile = async () => {
    setBusy("compile");
    setError("");
    setResult(null);
    try {
      const response = await postJson<any>(API.orchestrator.serviceChainCompile, body(), 30_000);
      setResult(response);
      toast("服务链编译成功", "success");
    } catch (failed: any) {
      setError(failed?.status === 404 ? "当前运行时未注册独立的 service-chains 端点（404）；透明编排由拓扑/策略保存同步应用。" : failed?.message || "服务链编译失败");
    } finally {
      setBusy("");
    }
  };

  const apply = async () => {
    setBusy("apply");
    setError("");
    setResult(null);
    try {
      const response = await postJson<any>(API.orchestrator.serviceChainApply, { ...body(), attachments: [] }, 30_000);
      setResult(response);
      toast("服务链已应用", "success");
    } catch (failed: any) {
      setError(failed?.status === 404 ? "当前运行时未注册独立的 service-chains 端点（404）；透明编排由拓扑/策略保存同步应用。" : failed?.message || "服务链应用失败");
      toast(failed?.message || "服务链应用失败", "error");
    } finally {
      setBusy("");
    }
  };

  return (
    <div className="page">
      <PageHeader
        title="透明编排"
        subtitle="单机服务链：编译 LAN 出口路径、应用与当前操作；拓扑/策略保存后自动下发透明编排"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { operation.refetch(); topology.refetch(); }}>刷新</Button><Button size="sm" icon={<Zap size={14} />} loading={busy === "compile"} onClick={compile} disabled={!topology.data?.item}>编译</Button><Button size="sm" variant="primary" icon={<Play size={14} />} loading={busy === "apply"} onClick={apply} disabled={session?.role === "readonly" || !topology.data?.item}>应用服务链</Button></>}
      />

      <div className="status-strip">
        <span>当前操作 <Badge tone={op.state === "running" ? "warning" : "neutral"} dot>{stateLabel(op.state || "idle")}</Badge></span>
        <span>名称 <b>{text(op.name, "—")}</b></span>
        <span>开始 <b>{formatDateTime(op.started_at)}</b></span>
        <span>透明编排 <b>随拓扑/策略同步应用</b></span>
      </div>

      <div className="inline-warning">独立 service-chains 端点仅在运行时未实现 TransparentRuntime 时注册；当前运行时通过拓扑/策略保存同步应用透明编排。页面保留编译/应用入口，若端点未注册会显示 404，不伪造结果。</div>
      <div className="orch-transparent-grid">
        <Card title="Flow" subtitle="编译输入；端口要求按协议填写" actions={<Badge tone="info">{String(flow.protocol || "tcp").toUpperCase()}</Badge>}>
          <div className="form-grid">
            <Field label="源 IP" required><Input value={flow.source_ip || ""} onChange={(event) => setFlow({ ...flow, source_ip: event.target.value })} placeholder="192.168.10.10" /></Field>
            <Field label="目的 IP" required><Input value={flow.destination_ip || ""} onChange={(event) => setFlow({ ...flow, destination_ip: event.target.value })} placeholder="203.0.113.10" /></Field>
            <Field label="协议"><Select value={flow.protocol || "tcp"} onChange={(event) => setFlow({ ...flow, protocol: event.target.value })}><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option><option value="icmpv6">ICMPv6</option></Select></Field>
            <Field label="源端口"><Input type="number" value={flow.source_port || ""} onChange={(event) => setFlow({ ...flow, source_port: event.target.value })} /></Field>
            <Field label="目的端口"><Input type="number" value={flow.destination_port || ""} onChange={(event) => setFlow({ ...flow, destination_port: event.target.value })} /></Field>
          </div>
        </Card>

        <Card title="服务链绑定" subtitle="按编排组顺序；每段需要 WAN/LAN 侧下一跳" actions={<Button size="sm" variant="text" onClick={() => setBindings([...bindings, { group: "", wan_facing_next_hop: "", lan_facing_next_hop: "" }])}>新增段</Button>}>
          {bindings.map((binding, index) => (
            <div className="binding-row" key={index}>
              <Field label={`第 ${index + 1} 段编排组`}><Select value={binding.group} onChange={(event) => setBindings(bindings.map((item, position) => position === index ? { ...item, group: event.target.value } : item))}><option value="">请选择</option>{groups.map((group) => <option key={group.name} value={group.name}>{group.name}</option>)}</Select></Field>
              <Field label="WAN 侧下一跳"><Input value={binding.wan_facing_next_hop} onChange={(event) => setBindings(bindings.map((item, position) => position === index ? { ...item, wan_facing_next_hop: event.target.value } : item))} placeholder="192.0.2.1" /></Field>
              <Field label="LAN 侧下一跳"><Input value={binding.lan_facing_next_hop} onChange={(event) => setBindings(bindings.map((item, position) => position === index ? { ...item, lan_facing_next_hop: event.target.value } : item))} placeholder="192.168.10.1" /></Field>
              <Button size="sm" variant="text" onClick={() => setBindings(bindings.filter((_, position) => position !== index))} disabled={bindings.length <= 1}>移除</Button>
            </div>
          ))}
        </Card>
      </div>

      {error && <div className="error-state"><strong>服务链操作失败</strong><p>{error}</p></div>}
      {result && <Card title="编译 / 应用结果" subtitle="后端返回的真实 path / chain"><pre className="json-preview">{JSON.stringify(result, null, 2)}</pre></Card>}
      {!groups.length && <div className="inline-warning">当前没有编排组；服务链需要至少一个编排组。请先在“单机拓扑”配置。</div>}

      <div className="inline-warning"><Workflow size={14} /> 透明编排是单机本地 L3 服务链，不是多站点控制器；服务链 apply 依赖运行时，未配置时会返回明确错误，不会伪造成功。</div>
    </div>
  );
}
