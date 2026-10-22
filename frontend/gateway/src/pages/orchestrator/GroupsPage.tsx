import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RefreshCw, Trash2, Waypoints } from "lucide-react";
import { API } from "../../api/endpoints";
import { deleteJson, postJson, putJson } from "../../api/client";
import { useInterfacesResource } from "../../api/hooks";
import { useOrchestratorTopology } from "../../api/orchestrator-hooks";
import type { OrchestrationGroup } from "../../api/types";
import { DataTable } from "../../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, useToast } from "../../components/ui";
import { useAuth, useConfirm } from "../../state/providers";
import { text } from "../../lib/format";

function portOf(group: OrchestrationGroup, direction: "lan_facing" | "wan_facing") {
  return group.ports.find((port) => port.direction === direction)?.interface || "";
}

export function OrchestratorGroupsPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const topology = useOrchestratorTopology();
  const inventory = useInterfacesResource();
  const [editor, setEditor] = useState<OrchestrationGroup | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const groups = topology.data?.item?.orchestration_groups || [];
  const management = topology.data?.item?.management_interface || "";
  const usedPorts = useMemo(() => {
    const used = new Set<string>();
    for (const item of topology.data?.item?.interfaces || []) if (item.port) used.add(item.port);
    for (const group of groups) for (const port of group.ports) if (port.interface) used.add(port.interface);
    return used;
  }, [topology.data, groups]);

  const open = (group?: OrchestrationGroup) => {
    setForm({
      name: group?.name || "",
      lan: group ? portOf(group, "lan_facing") : "",
      wan: group ? portOf(group, "wan_facing") : "",
    });
    setEditor(group || null);
  };

  const save = async () => {
    if (!form.name?.trim() || !form.lan || !form.wan) { toast("请填写组名称与两侧端口", "error"); return; }
    setBusy(true);
    try {
      const payload = {
        name: form.name.trim(),
        ports: [
          { interface: form.lan, direction: "lan_facing" },
          { interface: form.wan, direction: "wan_facing" },
        ],
      };
      if (editor) await putJson(API.orchestrator.group(editor.name), payload, 30_000);
      else await postJson(API.orchestrator.groups, payload, 30_000);
      toast("编排组已保存", "success");
      setEditor(undefined);
      await queryClient.invalidateQueries({ queryKey: ["orchestrator"] });
    } catch (error: any) {
      toast(error?.message || "保存编排组失败", "error", error?.requestId);
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
      toast(error?.message || "删除编排组失败", "error", error?.requestId);
    }
  };

  const columns: ColumnDef<OrchestrationGroup, any>[] = [
    { id: "name", header: "组名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><Waypoints size={14} /><strong>{text(row.original.name)}</strong></span>, meta: { width: 200 } },
    { id: "lan", header: "LAN-facing", accessorFn: (row) => portOf(row, "lan_facing"), cell: ({ getValue }) => <Badge tone="success">{text(getValue())}</Badge>, meta: { width: 140 } },
    { id: "wan", header: "WAN-facing", accessorFn: (row) => portOf(row, "wan_facing"), cell: ({ getValue }) => <Badge tone="info">{text(getValue())}</Badge>, meta: { width: 140 } },
    { id: "state", header: "路径", accessorFn: () => "2 端口", cell: () => <span className="muted">LAN → 服务 → WAN</span> },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ];

  const candidates = (inventory.data?.items || []).filter((item) => {
    const name = item.name || item.id;
    return name !== management && (!usedPorts.has(name) || name === form.lan || name === form.wan);
  });

  return (
    <div className="page">
      <PageHeader
        title="编排组"
        subtitle="每组两个方向端口；策略 via 动作按组进入服务链"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { topology.refetch(); inventory.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly" || !topology.data?.item}>新增编排组</Button></>}
      />
      {!topology.data?.item && <div className="inline-warning">请先在“单机拓扑”中配置 LAN/WAN 并保存，再创建编排组。</div>}
      <DataTable data={groups} columns={columns} loading={topology.isLoading} error={topology.isError ? topology.error : null} onRetry={() => topology.refetch()} emptyTitle="暂无编排组" emptyDescription="没有编排组时，策略只能 direct/drop，无法经过服务链。" emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly" || !topology.data?.item}>新增编排组</Button>} getRowId={(row) => row.name} />

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor ? "编辑编排组" : "新增编排组"} size="md" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存</Button></>}>
        <div className="form-grid">
          <Field label="组名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="LAN-facing 端口" required><Select value={form.lan || ""} onChange={(event) => setForm({ ...form, lan: event.target.value })}><option value="">请选择</option>{candidates.map((item) => <option key={item.id} value={item.name || item.id}>{item.name || item.id}</option>)}</Select></Field>
          <Field label="WAN-facing 端口" required><Select value={form.wan || ""} onChange={(event) => setForm({ ...form, wan: event.target.value })}><option value="">请选择</option>{candidates.map((item) => <option key={item.id} value={item.name || item.id}>{item.name || item.id}</option>)}</Select></Field>
        </div>
        <div className="inline-warning">编排组不能使用管理口、逻辑 LAN/WAN 端口或链路聚合成员；保存失败会返回具体原因。</div>
      </Modal>
    </div>
  );
}
