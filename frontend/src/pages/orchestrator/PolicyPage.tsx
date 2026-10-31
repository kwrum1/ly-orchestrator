import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, GitBranch, Pencil, Plus, Save, Trash2, Zap } from "lucide-react";
import { API } from "../../api/endpoints";
import { postJson, putJson } from "../../api/client";
import { useOrchestratorPolicy, useOrchestratorTopology } from "../../api/orchestrator-hooks";
import type { OrchestratorPolicy, PolicyAction, PolicyGroup, PolicyIPObject, PolicyRule } from "../../api/types";
import { Badge, Button, Card, EmptyState, Field, IconButton, Input, Modal, PageHeader, Select, Textarea, useToast } from "../../components/ui";
import { useAuth, useConfirm } from "../../state/providers";
import { text } from "../../lib/format";

function emptyPolicy(): OrchestratorPolicy {
  return { schema_version: 1, ip_objects: [], policy_groups: [], default: { kind: "direct" } };
}

function portRanges(value: string) {
  return value.split(/[\s,]+/).map((item) => item.trim()).filter(Boolean).map((item) => {
    const [start, end] = item.split("-").map(Number);
    return { start: start || end || 0, end: end || start || 0 };
  }).filter((range) => range.start > 0 && range.end > 0);
}

function portText(ranges?: { start: number; end: number }[]) {
  return (ranges || []).map((range) => range.start === range.end ? String(range.start) : `${range.start}-${range.end}`).join(",");
}

function actionLabel(action?: PolicyAction) {
  if (!action) return "--";
  if (action.kind === "via") return `via ${action.group || "?"}`;
  return action.kind === "direct" ? "直通" : action.kind === "drop" ? "丢弃" : action.kind;
}

function actionTone(action?: PolicyAction) {
  if (!action) return "neutral";
  if (action.kind === "via") return "violet";
  if (action.kind === "drop") return "danger";
  return "success";
}

// The collection order is the execution order. position/sequence are derived by
// the server from this array order, so they must never be part of the payload.
function policyPayload(policy: OrchestratorPolicy) {
  return {
    schema_version: policy.schema_version,
    ip_objects: policy.ip_objects.map((object) => ({ id: object.id, prefixes: object.prefixes })),
    policy_groups: policy.policy_groups.map((group) => ({
      id: group.id,
      rules: group.rules.map((rule) => ({ id: rule.id, match: rule.match, action: rule.action })),
    })),
    default: policy.default,
  };
}

function moveItem<T>(items: T[], from: number, to: number): T[] {
  if (from === to || to < 0 || to >= items.length) return items;
  const next = [...items];
  const [item] = next.splice(from, 1);
  next.splice(to, 0, item);
  return next;
}

export function OrchestratorPolicyPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const readonly = session?.role === "readonly";
  const policyQuery = useOrchestratorPolicy();
  const topology = useOrchestratorTopology();
  const [draft, setDraft] = useState<OrchestratorPolicy>(emptyPolicy());
  const [selectedId, setSelectedId] = useState("");
  const [busy, setBusy] = useState(false);
  const [groupEditor, setGroupEditor] = useState<PolicyGroup | null | undefined>(undefined);
  const [groupForm, setGroupForm] = useState<any>({});
  const [ruleEditor, setRuleEditor] = useState<PolicyRule | null | undefined>(undefined);
  const [ruleForm, setRuleForm] = useState<any>({});
  const [objectEditor, setObjectEditor] = useState<PolicyIPObject | null | undefined>(undefined);
  const [objectForm, setObjectForm] = useState<any>({});
  const [compileForm, setCompileForm] = useState<any>({ protocol: "tcp", source_port: "12345", destination_port: "443" });
  const [compileResult, setCompileResult] = useState<any>(null);
  const [compileError, setCompileError] = useState("");

  useEffect(() => {
    const next = policyQuery.data?.item || emptyPolicy();
    setDraft(next);
    setSelectedId((current) => current || next.policy_groups[0]?.id || "");
  }, [policyQuery.data]);

  const groups = draft.policy_groups || [];
  const selected = groups.find((group) => group.id === selectedId) || groups[0];
  const topologyGroups = topology.data?.item?.orchestration_groups || [];

  const updateGroup = (group: PolicyGroup) => {
    setDraft({ ...draft, policy_groups: groups.map((item) => item.id === group.id ? group : item) });
  };

  const moveGroup = (index: number, delta: number) => {
    const next = moveItem(groups, index, index + delta);
    if (next === groups) return;
    setDraft({ ...draft, policy_groups: next });
  };

  const moveRule = (index: number, delta: number) => {
    if (!selected) return;
    const next = moveItem(selected.rules, index, index + delta);
    if (next === selected.rules) return;
    updateGroup({ ...selected, rules: next });
  };

  const save = async () => {
    if (!topology.data?.item) { toast("请先保存单机拓扑", "error"); return; }
    if (draft.default.kind === "via" && !draft.default.group) { toast("默认动作 via 需要选择编排组", "error"); return; }
    for (const group of groups) {
      for (const rule of group.rules) {
        if (rule.action.kind === "via" && !rule.action.group) { toast(`规则 ${group.id}/${rule.id} 需要选择编排组`, "error"); return; }
      }
    }
    setBusy(true);
    try {
      await putJson(API.orchestrator.policy, policyPayload(draft), 30_000);
      toast("流量编排策略已保存", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator"] });
    } catch (error: any) {
      toast(error?.message || "保存策略失败", "error");
    } finally {
      setBusy(false);
    }
  };

  const openGroup = (group?: PolicyGroup) => {
    setGroupForm({ id: group?.id || `group-${groups.length + 1}` });
    setGroupEditor(group || null);
  };

  const saveGroup = () => {
    if (!groupForm.id?.trim()) { toast("请填写策略组 ID", "error"); return; }
    if (groupEditor) {
      updateGroup({ ...groupEditor, id: groupForm.id.trim() });
    } else {
      const next: PolicyGroup = { id: groupForm.id.trim(), rules: [] };
      setDraft({ ...draft, policy_groups: [...groups, next] });
      setSelectedId(next.id);
    }
    setGroupEditor(undefined);
  };

  const removeGroup = async (group: PolicyGroup) => {
    if (!(await confirm({ title: `删除策略组 ${group.id}？`, message: "组内规则会一起删除；保存后生效。", confirmLabel: "删除", danger: true }))) return;
    setDraft({ ...draft, policy_groups: groups.filter((item) => item.id !== group.id) });
    if (selectedId === group.id) setSelectedId(groups.find((item) => item.id !== group.id)?.id || "");
  };

  const openRule = (rule?: PolicyRule) => {
    const match = rule?.match;
    setRuleForm({
      id: rule?.id || `rule-${(selected?.rules.length || 0) + 1}`,
      sources: (match?.sources || []).join("\n") || "any",
      destinations: (match?.destinations || []).join("\n") || "any",
      protocol: match?.protocol || "any",
      sourcePorts: portText(match?.source_ports),
      destinationPorts: portText(match?.destination_ports),
      actionKind: rule?.action.kind || "direct",
      actionGroup: rule?.action.group || topologyGroups[0]?.name || "",
    });
    setRuleEditor(rule || null);
  };

  const saveRule = () => {
    if (!selected) return;
    if (!ruleForm.id?.trim()) { toast("请填写规则 ID", "error"); return; }
    if (ruleForm.actionKind === "via" && !ruleForm.actionGroup) { toast("via 动作需要选择编排组", "error"); return; }
    const rule: PolicyRule = {
      id: ruleForm.id.trim(),
      match: {
        sources: String(ruleForm.sources || "any").split(/\s+/).filter(Boolean),
        destinations: String(ruleForm.destinations || "any").split(/\s+/).filter(Boolean),
        protocol: ruleForm.protocol || "any",
        source_ports: portRanges(ruleForm.sourcePorts || ""),
        destination_ports: portRanges(ruleForm.destinationPorts || ""),
      },
      action: ruleForm.actionKind === "via" ? { kind: "via", group: ruleForm.actionGroup } : { kind: ruleForm.actionKind },
    };
    const rules = ruleEditor
      ? selected.rules.map((item) => item.id === ruleEditor.id ? rule : item)
      : [...selected.rules, rule];
    updateGroup({ ...selected, rules });
    setRuleEditor(undefined);
  };

  const removeRule = (rule: PolicyRule) => {
    if (!selected) return;
    updateGroup({ ...selected, rules: selected.rules.filter((item) => item.id !== rule.id) });
  };

  const openObject = (object?: PolicyIPObject) => {
    setObjectForm({ id: object?.id || `ip-${draft.ip_objects.length + 1}`, prefixes: (object?.prefixes || []).join("\n") });
    setObjectEditor(object || null);
  };

  const saveObject = () => {
    if (!objectForm.id?.trim()) { toast("请填写 IP 对象 ID", "error"); return; }
    const object = { id: objectForm.id.trim(), prefixes: String(objectForm.prefixes || "").split(/\s+/).filter(Boolean) };
    const next = objectEditor ? draft.ip_objects.map((item) => item.id === objectEditor.id ? object : item) : [...draft.ip_objects, object];
    setDraft({ ...draft, ip_objects: next });
    setObjectEditor(undefined);
  };

  const compile = async () => {
    setCompileError("");
    setCompileResult(null);
    try {
      const result = await postJson<any>(API.orchestrator.policyCompile, {
        flow: {
          source_ip: compileForm.source_ip,
          destination_ip: compileForm.destination_ip,
          protocol: compileForm.protocol,
          source_port: Number(compileForm.source_port) || 0,
          destination_port: Number(compileForm.destination_port) || 0,
        },
        prelude: {},
      }, 30_000);
      setCompileResult(result);
    } catch (error: any) {
      setCompileError(error?.message || "编译失败");
    }
  };

  return (
    <div className="page">
      <PageHeader
        title="流量编排"
        subtitle="单机本地策略：IP 对象、策略组、规则与默认动作；保存即同步透明编排"
        actions={<><Button size="sm" variant="primary" icon={<Save size={14} />} loading={busy} onClick={save} disabled={readonly || !topology.data?.item}>保存策略</Button></>}
        meta={<><span className="meta-item">策略 checksum {text(policyQuery.data?.checksum, "未保存")}</span><span className="meta-item">拓扑 {topology.data?.item ? "已配置" : "未配置"}</span></>}
      />
      {!topology.data?.item && <div className="inline-warning">策略保存依赖单机拓扑；请先在“单机拓扑”保存 LAN/WAN。</div>}

      <Card title="默认动作" subtitle="未匹配任何策略组时的行为" actions={<Badge tone={actionTone(draft.default)}>{actionLabel(draft.default)}</Badge>}>
        <div className="form-grid">
          <Field label="默认动作"><Select value={draft.default.kind} onChange={(event) => setDraft({ ...draft, default: { kind: event.target.value, group: event.target.value === "via" ? topologyGroups[0]?.name || "" : undefined } })}><option value="direct">直通（LAN/WAN）</option><option value="drop">丢弃</option><option value="via">经过编排组</option></Select></Field>
          {draft.default.kind === "via" && <Field label="编排组"><Select value={draft.default.group || ""} onChange={(event) => setDraft({ ...draft, default: { ...draft.default, group: event.target.value } })}><option value="">请选择</option>{topologyGroups.map((group) => <option key={group.name} value={group.name}>{group.name}</option>)}</Select></Field>}
        </div>
      </Card>

      <div className="orch-policy-grid">
        <Card title="策略组" subtitle={`${groups.length} 个组 · 自上而下匹配`} actions={<Button size="sm" icon={<Plus size={14} />} onClick={() => openGroup()} disabled={readonly}>新增组</Button>}>
          <div className="orch-policy-list">
            {groups.map((group, index) => (
              <div key={group.id} className="orch-policy-row" data-policy-group={group.id}>
                <button type="button" className={selected?.id === group.id ? "orch-policy-item is-active" : "orch-policy-item"} onClick={() => setSelectedId(group.id)}>
                  <span><strong>{group.id}</strong><small>{group.rules.length} 条规则</small></span>
                  <Badge tone="neutral">{group.rules.length}</Badge>
                </button>
                <div className="orch-order-actions">
                  <IconButton label={`上移策略组 ${group.id}`} onClick={() => moveGroup(index, -1)} disabled={readonly || index === 0}><ArrowUp size={13} /></IconButton>
                  <IconButton label={`下移策略组 ${group.id}`} onClick={() => moveGroup(index, 1)} disabled={readonly || index === groups.length - 1}><ArrowDown size={13} /></IconButton>
                </div>
              </div>
            ))}
            {!groups.length && <EmptyState title="暂无策略组" description="创建策略组后添加规则。" icon={<GitBranch size={22} />} />}
          </div>
        </Card>

        <Card title={selected ? `规则 · ${selected.id}` : "规则"} subtitle={selected ? "组内规则按列表顺序自上而下匹配" : "请选择策略组"} actions={selected ? <><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openGroup(selected)} disabled={readonly}>编辑组</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => removeGroup(selected)} disabled={readonly}>删除组</Button><Button size="sm" icon={<Plus size={14} />} onClick={() => openRule()} disabled={readonly}>新增规则</Button></> : undefined}>
          {selected ? (
            <div className="mini-table-wrap">
              <table className="data-table">
                <thead><tr><th>源 / 目的</th><th>协议 / 端口</th><th>动作</th><th>操作</th></tr></thead>
                <tbody>
                  {selected.rules.map((rule, index) => (
                    <tr key={rule.id} data-policy-rule={rule.id}>
                      <td className="mono truncate">{(rule.match.sources || []).join(",") || "any"} → {(rule.match.destinations || []).join(",") || "any"}</td>
                      <td>{String(rule.match.protocol || "any").toUpperCase()} · {portText(rule.match.source_ports) || "any"}/{portText(rule.match.destination_ports) || "any"}</td>
                      <td><Badge tone={actionTone(rule.action)}>{actionLabel(rule.action)}</Badge></td>
                      <td><div className="row-actions">
                        <IconButton label={`上移规则 ${rule.id}`} onClick={() => moveRule(index, -1)} disabled={readonly || index === 0}><ArrowUp size={13} /></IconButton>
                        <IconButton label={`下移规则 ${rule.id}`} onClick={() => moveRule(index, 1)} disabled={readonly || index === selected.rules.length - 1}><ArrowDown size={13} /></IconButton>
                        <Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openRule(rule)} disabled={readonly}>编辑</Button>
                        <Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => removeRule(rule)} disabled={readonly}>删除</Button>
                      </div></td>
                    </tr>
                  ))}
                </tbody>
              </table>
              {!selected.rules.length && <EmptyState title="暂无规则" description="该组会直接落到下一个组或默认动作。" icon={<GitBranch size={22} />} />}
            </div>
          ) : <EmptyState title="请选择策略组" icon={<GitBranch size={22} />} />}
        </Card>
      </div>

      <div className="orch-policy-grid">
        <Card title="IP 对象" subtitle="规则中的源/目的可引用对象 ID 或直接写前缀" actions={<Button size="sm" icon={<Plus size={14} />} onClick={() => openObject()} disabled={readonly}>新增对象</Button>}>
          <div className="orch-object-list">
            {draft.ip_objects.map((object) => (
              <div className="orch-object" key={object.id}><div><strong>{object.id}</strong><small>{object.prefixes.join(", ") || "空前缀"}</small></div><div><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openObject(object)} disabled={readonly}>编辑</Button></div></div>
            ))}
            {!draft.ip_objects.length && <p className="muted">暂无 IP 对象；规则可直接使用 CIDR。</p>}
          </div>
        </Card>

        <Card title="策略编译测试" subtitle="POST /policy/compile；只读，不修改策略" actions={<Button size="sm" icon={<Zap size={14} />} onClick={compile}>编译</Button>}>
          <div className="form-grid">
            <Field label="源 IP"><Input value={compileForm.source_ip || ""} onChange={(event) => setCompileForm({ ...compileForm, source_ip: event.target.value })} placeholder="192.168.10.10" /></Field>
            <Field label="目的 IP"><Input value={compileForm.destination_ip || ""} onChange={(event) => setCompileForm({ ...compileForm, destination_ip: event.target.value })} placeholder="203.0.113.10" /></Field>
            <Field label="协议"><Select value={compileForm.protocol || "tcp"} onChange={(event) => setCompileForm({ ...compileForm, protocol: event.target.value })}><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option><option value="icmpv6">ICMPv6</option></Select></Field>
            <Field label="源端口"><Input type="number" value={compileForm.source_port || ""} onChange={(event) => setCompileForm({ ...compileForm, source_port: event.target.value })} /></Field>
            <Field label="目的端口"><Input type="number" value={compileForm.destination_port || ""} onChange={(event) => setCompileForm({ ...compileForm, destination_port: event.target.value })} /></Field>
          </div>
          {compileError && <div className="error-state"><strong>编译失败</strong><p>{compileError}</p></div>}
          {compileResult && <pre className="json-preview">{JSON.stringify(compileResult, null, 2)}</pre>}
        </Card>
      </div>

      <Modal open={groupEditor !== undefined} onClose={() => setGroupEditor(undefined)} title={groupEditor ? "编辑策略组" : "新增策略组"} size="sm" footer={<><Button onClick={() => setGroupEditor(undefined)}>取消</Button><Button variant="primary" onClick={saveGroup}>保存</Button></>}>
        <div className="form-stack">
          <Field label="组 ID" required hint="新组默认追加到列表末尾，可用上移/下移调整匹配顺序"><Input value={groupForm.id || ""} onChange={(event) => setGroupForm({ ...groupForm, id: event.target.value })} /></Field>
        </div>
      </Modal>

      <Modal open={ruleEditor !== undefined} onClose={() => setRuleEditor(undefined)} title={ruleEditor ? "编辑策略规则" : "新增策略规则"} size="lg" footer={<><Button onClick={() => setRuleEditor(undefined)}>取消</Button><Button variant="primary" onClick={saveRule}>保存</Button></>}>
        <div className="form-grid">
          <Field label="规则 ID" required hint="新规则默认追加到组内末尾，可用上移/下移调整匹配顺序"><Input value={ruleForm.id || ""} onChange={(event) => setRuleForm({ ...ruleForm, id: event.target.value })} /></Field>
          <Field label="源地址" className="span-2" hint="每行一个对象 ID 或 CIDR；any 表示任意"><Textarea rows={3} value={ruleForm.sources || "any"} onChange={(event) => setRuleForm({ ...ruleForm, sources: event.target.value })} /></Field>
          <Field label="目的地址" className="span-2" hint="每行一个对象 ID 或 CIDR；any 表示任意"><Textarea rows={3} value={ruleForm.destinations || "any"} onChange={(event) => setRuleForm({ ...ruleForm, destinations: event.target.value })} /></Field>
          <Field label="协议"><Select value={ruleForm.protocol || "any"} onChange={(event) => setRuleForm({ ...ruleForm, protocol: event.target.value })}><option value="any">Any</option><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option><option value="icmpv6">ICMPv6</option></Select></Field>
          <Field label="动作"><Select value={ruleForm.actionKind || "direct"} onChange={(event) => setRuleForm({ ...ruleForm, actionKind: event.target.value })}><option value="direct">直通</option><option value="drop">丢弃</option><option value="via">经过编排组</option></Select></Field>
          {ruleForm.actionKind === "via" && <Field label="编排组" required><Select value={ruleForm.actionGroup || ""} onChange={(event) => setRuleForm({ ...ruleForm, actionGroup: event.target.value })}><option value="">请选择</option>{topologyGroups.map((group) => <option key={group.name} value={group.name}>{group.name}</option>)}</Select></Field>}
          <Field label="源端口"><Input value={ruleForm.sourcePorts || ""} onChange={(event) => setRuleForm({ ...ruleForm, sourcePorts: event.target.value })} placeholder="80,8000-8010" /></Field>
          <Field label="目的端口"><Input value={ruleForm.destinationPorts || ""} onChange={(event) => setRuleForm({ ...ruleForm, destinationPorts: event.target.value })} placeholder="443" /></Field>
        </div>
      </Modal>

      <Modal open={objectEditor !== undefined} onClose={() => setObjectEditor(undefined)} title={objectEditor ? "编辑 IP 对象" : "新增 IP 对象"} size="sm" footer={<><Button onClick={() => setObjectEditor(undefined)}>取消</Button><Button variant="primary" onClick={saveObject}>保存</Button></>}>
        <div className="form-stack">
          <Field label="对象 ID" required><Input value={objectForm.id || ""} onChange={(event) => setObjectForm({ ...objectForm, id: event.target.value })} /></Field>
          <Field label="前缀" hint="每行一个 CIDR"><Textarea rows={5} value={objectForm.prefixes || ""} onChange={(event) => setObjectForm({ ...objectForm, prefixes: event.target.value })} /></Field>
        </div>
      </Modal>
    </div>
  );
}
