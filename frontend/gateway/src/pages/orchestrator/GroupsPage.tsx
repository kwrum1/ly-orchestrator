import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RefreshCw, Trash2, Waypoints } from "lucide-react";
import { API } from "../../api/endpoints";
import { deleteJson, postJson, putJson } from "../../api/client";
import { useInterfacesResource } from "../../api/hooks";
import { useOrchestratorTopology } from "../../api/orchestrator-hooks";
import type { OrchestrationGroup, Topology } from "../../api/types";
import { DataTable } from "../../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, useToast } from "../../components/ui";
import { useAuth, useConfirm } from "../../state/providers";
import { text } from "../../lib/format";

type GroupForm = { name: string; lan: string; wan: string };

const blankForm: GroupForm = { name: "", lan: "", wan: "" };

function normalizedDirection(value: unknown) {
  return String(value || "").trim().toLowerCase().replace(/-/g, "_");
}

function portOf(group: OrchestrationGroup | null | undefined, direction: "lan_facing" | "wan_facing") {
  if (!group) return "";
  const port = (group.ports || []).find((item) => normalizedDirection(item.direction) === direction);
  return port?.interface || port?.bond?.name || "";
}

function topologyPayload(topology: Topology, groups: OrchestrationGroup[], renameFrom: string, next: OrchestrationGroup) {
  return {
    schema_version: topology.schema_version || 1,
    management_interface: topology.management_interface,
    interfaces: (topology.interfaces || []).map((item) => item.bond?.members?.length
      ? { name: item.name, role: item.role, bond: item.bond }
      : { name: item.name, role: item.role, port: item.port }),
    orchestration_groups: groups.map((group) => group.name === renameFrom ? next : group).map((group) => ({
      name: group.name,
      ports: (group.ports || []).map((port) => port.bond?.members?.length
        ? { direction: port.direction, bond: port.bond }
        : { direction: port.direction, interface: port.interface }),
    })),
  };
}

export function OrchestratorGroupsPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const topology = useOrchestratorTopology();
  const inventory = useInterfacesResource();
  const [open, setOpen] = useState(false);
  const [editing, setEditing] = useState<OrchestrationGroup | null>(null);
  const [form, setForm] = useState<GroupForm>(blankForm);
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);

  const groups = topology.data?.item?.orchestration_groups || [];
  const management = topology.data?.item?.management_interface || "";
  const usedPorts = useMemo(() => {
    const used = new Set<string>();
    for (const item of topology.data?.item?.interfaces || []) if (item.port) used.add(item.port);
    for (const item of topology.data?.item?.interfaces || []) for (const member of item.bond?.members || []) used.add(member);
    for (const group of groups) {
      for (const port of group.ports || []) {
        if (port.interface) used.add(port.interface);
        for (const member of port.bond?.members || []) used.add(member);
      }
    }
    return used;
  }, [topology.data, groups]);

  const candidates = useMemo(() => {
    const values = new Map<string, string>();
    for (const item of inventory.data?.items || []) {
      const value = item.system_name || item.name || item.id;
      if (!value) continue;
      const label = (item.system_name && item.name && item.name !== item.system_name ? `${item.name} · ${item.system_name}` : (item.name || item.id || item.system_name)) || value;
      values.set(value, label);
    }
    for (const group of groups) for (const port of group.ports || []) {
      if (port.interface) values.set(port.interface, values.get(port.interface) || port.interface);
    }
    if (form.lan) values.set(form.lan, values.get(form.lan) || form.lan);
    if (form.wan) values.set(form.wan, values.get(form.wan) || form.wan);
    return [...values.entries()].filter(([value]) => value !== management && (!usedPorts.has(value) || value === form.lan || value === form.wan));
  }, [inventory.data, groups, form.lan, form.wan, management, usedPorts]);

  const openEditor = (group?: OrchestrationGroup) => {
    const next = group || null;
    const lan = portOf(next, "lan_facing");
    const wan = portOf(next, "wan_facing");
    setEditing(next);
    setForm({ name: next?.name || "", lan, wan });
    setFormError("");
    setOpen(true);
  };

  const closeEditor = () => {
    if (busy) return;
    setOpen(false);
    setEditing(null);
    setForm(blankForm);
    setFormError("");
  };

  const save = async () => {
    const name = form.name.trim();
    if (!name) { setFormError("请填写编排组名称"); return; }
    if (!form.lan || !form.wan) { setFormError("请选择 LAN-facing 与 WAN-facing 端口"); return; }
    if (form.lan === form.wan) { setFormError("LAN-facing 与 WAN-facing 不能使用同一个端口"); return; }
    const nextGroup: OrchestrationGroup = {
      name,
      ports: [
        { interface: form.lan, direction: "lan_facing" },
        { interface: form.wan, direction: "wan_facing" },
      ],
    };
    setBusy(true);
    setFormError("");
    try {
      if (editing) {
        if (name !== editing.name) {
          const current = topology.data?.item;
          if (!current) throw new Error("拓扑未加载，无法改名");
          await putJson(API.orchestrator.topology, topologyPayload(current, groups, editing.name, nextGroup), 30_000);
        } else {
          await putJson(API.orchestrator.group(editing.name), nextGroup, 30_000);
        }
      } else {
        await postJson(API.orchestrator.groups, nextGroup, 30_000);
      }
      toast("编排组已保存", "success");
      closeEditor();
      await queryClient.invalidateQueries({ queryKey: ["orchestrator"] });
    } catch (error: any) {
      const message = error?.message || "保存编排组失败";
      setFormError(message);
      toast(message, "error");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (group: OrchestrationGroup) => {
    if (!(await confirm({ title: `删除编排组 ${group.name}？`, message: "引用该组的策略会失效；后端会拒绝或回滚。", confirmLabel: "删除", danger: true }))) return;
    try {
      await deleteJson(API.orchestrator.group(group.name), 30_000);
      toast("编排组已删除", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator"] });
    } catch (error: any) {
      toast(error?.message || "删除编排组失败", "error");
    }
  };

  const columns: ColumnDef<OrchestrationGroup, any>[] = [
    { id: "name", header: "组名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><Waypoints size={14} /><strong>{text(row.original.name)}</strong></span>, meta: { width: 200 } },
    { id: "lan", header: "LAN-facing", accessorFn: (row) => portOf(row, "lan_facing"), cell: ({ getValue }) => <Badge tone="success">{text(getValue(), "--")}</Badge>, meta: { width: 140 } },
    { id: "wan", header: "WAN-facing", accessorFn: (row) => portOf(row, "wan_facing"), cell: ({ getValue }) => <Badge tone="info">{text(getValue(), "--")}</Badge>, meta: { width: 140 } },
    { id: "state", header: "路径", accessorFn: () => "2 端口", cell: () => <span className="muted">LAN → 服务 → WAN</span> },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openEditor(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ];

  return (
    <div className="page">
      <PageHeader
        title="编排组"
        subtitle="每组两个方向端口；策略 via 动作按组进入服务链"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { topology.refetch(); inventory.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => openEditor()} disabled={session?.role === "readonly" || !topology.data?.item}>新增编排组</Button></>}
      />
      {!topology.data?.item && <div className="inline-warning">请先在“单机拓扑”中配置 LAN/WAN 并保存，再创建编排组。</div>}
      <DataTable data={groups} columns={columns} loading={topology.isLoading} error={topology.isError ? topology.error : null} onRetry={() => topology.refetch()} emptyTitle="暂无编排组" emptyDescription="没有编排组时，策略只能 direct/drop，无法经过服务链。" emptyAction={<Button size="sm" variant="primary" onClick={() => openEditor()} disabled={session?.role === "readonly" || !topology.data?.item}>新增编排组</Button>} getRowId={(row) => row.name} />

      <Modal open={open} onClose={closeEditor} title={editing ? "编辑编排组" : "新增编排组"} size="md" busy={busy} footer={<><Button onClick={closeEditor} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存</Button></>}>
        <div className="form-grid">
          <Field label="组名称" required hint={editing ? "名称是 API 主键，编辑时不可修改；端口会保留" : undefined}><Input value={form.name} readOnly={Boolean(editing)} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="LAN-facing 端口" required><Select value={form.lan} onChange={(event) => setForm({ ...form, lan: event.target.value })}><option value="">请选择</option>{candidates.map(([value, label]) => <option key={`lan-${value}`} value={value}>{label}</option>)}</Select></Field>
          <Field label="WAN-facing 端口" required><Select value={form.wan} onChange={(event) => setForm({ ...form, wan: event.target.value })}><option value="">请选择</option>{candidates.map(([value, label]) => <option key={`wan-${value}`} value={value}>{label}</option>)}</Select></Field>
        </div>
        {formError && <div className="error-state" role="alert"><strong>保存失败</strong><p>{formError}</p></div>}
        <div className="inline-warning">编排组不能使用管理口、逻辑 LAN/WAN 端口或已占用端口；保存失败会显示后端原因，不会保留假成功状态。</div>
      </Modal>
    </div>
  );
}
