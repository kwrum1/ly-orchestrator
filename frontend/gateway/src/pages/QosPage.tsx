import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { CircleGauge, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useObjectGroups, useTrafficControl } from "../api/hooks";
import type { TrafficControlRule } from "../api/types";
import { AddressListEditor } from "../components/AddressListEditor";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { splitTokens, stateLabel, text } from "../lib/format";

function ruleOf(item: TrafficControlRule) {
  return item.rules?.[0] || {};
}

function actionOf(item: TrafficControlRule) {
  const action = ruleOf(item).actions?.[0];
  return action?.kind === "drop" ? "阻断" : "限速";
}

function rateOf(item: TrafficControlRule) {
  const rate = Number(ruleOf(item).actions?.[0]?.policer?.rate_bps || 0);
  return rate > 0 ? `${(rate / 1_000_000).toFixed(2)} Mbps` : "--";
}

function list(value: unknown) {
  return Array.isArray(value) ? value.map(String) : [];
}

export function QosPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const rules = useTrafficControl();
  const objects = useObjectGroups();
  const [editor, setEditor] = useState<TrafficControlRule | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const open = (item?: TrafficControlRule) => {
    const rule = item ? ruleOf(item) : {};
    const match = rule.match || {};
    const action = rule.actions?.[0];
    setForm({
      priority: item?.id?.replace(/\D/g, "") || "",
      name: item?.name || "",
      sources: list(match.sources),
      destinations: list(match.destinations),
      sourcePorts: splitTokens((match.source_ports || []).join(",")).join(","),
      destPorts: splitTokens((match.dest_ports || []).join(",")).join(","),
      protocol: (match.protocols || ["any"])[0] || "any",
      action: action?.kind === "drop" ? "drop" : "limit",
      direction: match.direction || "both",
      rate: action?.policer?.rate_bps ? Number(action.policer.rate_bps) / 1_000_000 : "",
    });
    setEditor(item || null);
  };

  const save = async () => {
    if (!form.priority || Number(form.priority) < 1 || Number(form.priority) > 65535) { toast("策略序号必须在 1-65535 之间", "error"); return; }
    if (form.action === "limit" && !(Number(form.rate) > 0)) { toast("请填写限速大小", "error"); return; }
    setBusy(true);
    try {
      const id = editor?.id || `flow-${form.priority}`;
      const rate = Math.round(Number(form.rate || 0) * 1_000_000);
      const payload = {
        id,
        name: form.name || "",
        rules: [{
          id: `${id}-rule`,
          granularity: "rule",
          match: {
            sources: form.sources || [],
            destinations: form.destinations || [],
            source_ports: splitTokens(form.sourcePorts || ""),
            dest_ports: splitTokens(form.destPorts || ""),
            protocols: [form.protocol || "any"],
            direction: form.direction || "both",
          },
          actions: form.action === "drop" ? [{ kind: "drop" }] : [{ kind: "policer", policer: { rate_bps: rate, burst_bps: Math.max(8000, Math.round(rate / 10)) } }],
        }],
      };
      if (editor?.id) await patchJson(API.resources.trafficControlItem(id), payload);
      else await postJson(API.resources.trafficControl, payload);
      await queryClient.invalidateQueries({ queryKey: ["resources", "traffic-control"] });
      toast("流量控制已保存，正在应用", "success");
      setEditor(undefined);
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存流量控制失败", "error");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (item: TrafficControlRule) => {
    const ok = await confirm({ title: "删除流量控制规则？", message: "删除后该规则的限速/阻断立即失效。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      await deleteJson(API.resources.trafficControlItem(item.id));
      await queryClient.invalidateQueries({ queryKey: ["resources", "traffic-control"] });
      toast("已删除，正在应用", "success");
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error");
    }
  };

  const columns = useMemo<ColumnDef<TrafficControlRule, any>[]>(() => [
    { id: "priority", header: "序号", accessorFn: (row) => row.id, cell: ({ getValue }) => <strong className="mono">{text(getValue()).replace(/^flow-/, "")}</strong>, meta: { width: 90, align: "right" } },
    { id: "sources", header: "内 / 外地址", accessorFn: (row) => `${list(ruleOf(row).match?.sources).join(",") || "任意"} / ${list(ruleOf(row).match?.destinations).join(",") || "任意"}`, cell: ({ getValue }) => <span className="mono truncate" title={text(getValue())}>{text(getValue())}</span>, meta: { width: 220, mono: true } },
    { id: "ports", header: "内 / 外端口", accessorFn: (row) => `${list(ruleOf(row).match?.source_ports).join(",") || "任意"} / ${list(ruleOf(row).match?.dest_ports).join(",") || "任意"}`, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    { id: "protocol", header: "协议", accessorFn: (row) => (ruleOf(row).match?.protocols || ["any"])[0], cell: ({ getValue }) => <Badge tone="info">{String(getValue() || "any").toUpperCase()}</Badge>, meta: { width: 80 } },
    { id: "action", header: "动作", accessorFn: (row) => actionOf(row), cell: ({ getValue }) => <Badge tone={getValue() === "阻断" ? "danger" : "success"}>{text(getValue())}</Badge>, meta: { width: 80 } },
    { id: "direction", header: "方向", accessorFn: (row) => ruleOf(row).match?.direction, cell: ({ getValue }) => ({ both: "双向", uplink: "上行", downlink: "下行" } as any)[String(getValue())] || text(getValue(), "双向"), meta: { width: 80 } },
    { id: "rate", header: "限速", accessorFn: (row) => rateOf(row), cell: ({ getValue }) => text(getValue()), meta: { width: 110, align: "right" } },
    { id: "state", header: "状态", accessorFn: (row) => row.runtime_state, cell: ({ getValue }) => <Badge tone={getValue() === "applied" ? "success" : "warning"} dot>{stateLabel(getValue())}</Badge>, meta: { width: 90 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role]);

  return (
    <div className="page">
      <PageHeader
        title="流控"
        subtitle="按策略限速与阻断，保存即生效"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { rules.refetch(); objects.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly"}>新增</Button></>}
      />
      <DataTable data={rules.data?.items || []} columns={columns} loading={rules.isLoading} error={rules.isError ? rules.error : null} onRetry={() => rules.refetch()} emptyTitle="暂无流控规则" emptyDescription="创建限速或阻断规则，保存后立即生效。" emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly"}>新增规则</Button>} getRowId={(row) => row.id} />

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor ? "编辑流控" : "新增流控"} size="lg" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}>
        <div className="form-grid">
          <Field label="策略序号" required hint="1–65535"><Input type="number" min={1} max={65535} value={form.priority || ""} onChange={(event) => setForm({ ...form, priority: event.target.value })} /></Field>
          <Field label="备注"><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="源地址" className="span-2"><AddressListEditor value={form.sources || []} onChange={(value) => setForm({ ...form, sources: value })} groups={(objects.data?.items || []).filter((item) => item.kind === "ip")} /></Field>
          <Field label="目的地址" className="span-2"><AddressListEditor value={form.destinations || []} onChange={(value) => setForm({ ...form, destinations: value })} groups={(objects.data?.items || []).filter((item) => item.kind === "ip")} /></Field>
          <Field label="源端口"><Input value={form.sourcePorts || ""} onChange={(event) => setForm({ ...form, sourcePorts: event.target.value })} placeholder="任意" /></Field>
          <Field label="目的端口"><Input value={form.destPorts || ""} onChange={(event) => setForm({ ...form, destPorts: event.target.value })} placeholder="任意" /></Field>
          <Field label="协议"><Select value={form.protocol || "any"} onChange={(event) => setForm({ ...form, protocol: event.target.value })}><option value="any">Any</option><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option></Select></Field>
          <Field label="动作"><Select value={form.action || "limit"} onChange={(event) => setForm({ ...form, action: event.target.value })}><option value="limit">限速</option><option value="drop">阻断</option></Select></Field>
          <Field label="方向"><Select value={form.direction || "both"} onChange={(event) => setForm({ ...form, direction: event.target.value })}><option value="both">双向</option><option value="uplink">上行</option><option value="downlink">下行</option></Select></Field>
          {form.action !== "drop" && <Field label="限速 (Mbps)" required><Input type="number" min={0.064} step={0.001} value={form.rate || ""} onChange={(event) => setForm({ ...form, rate: event.target.value })} /></Field>}
        </div>
      </Modal>
    </div>
  );
}
