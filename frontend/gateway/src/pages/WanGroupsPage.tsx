import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RefreshCw, Trash2, Waypoints } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useWanGroups, useWanLinks } from "../api/hooks";
import type { WanGroup, WanLink } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { stateLabel, text } from "../lib/format";

const MODES = [
  ["primary_backup", "主备模式"],
  ["weighted", "权重负载"],
  ["flow_hash", "五元组负载"],
  ["least_connections", "最小连接数"],
  ["max_idle_bandwidth", "最大下行空闲带宽"],
];

function groupMembers(group: WanGroup) {
  return (group.members || group.wan_members || []).map(String);
}

export function WanGroupsPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const groups = useWanGroups();
  const links = useWanLinks();
  const [editor, setEditor] = useState<WanGroup | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);
  const wanItems = links.data?.items || [];

  const open = (group?: WanGroup) => {
    const members = group ? groupMembers(group) : [];
    setForm({
      name: group?.name || "",
      mode: group?.load_balance_mode || group?.mode || "primary_backup",
      hash: group?.hash_fields || group?.load_balance || "five_tuple",
      members,
      primary: group?.primary_member || members[0] || "",
      backup: group?.backup_member || members[1] || "",
      weights: group?.weights || {},
      connections: group?.connection_limits || {},
      downlink: group?.downlink_mbps || {},
    });
    setEditor(group || null);
  };

  const save = async () => {
    if (!form.name?.trim()) { toast("请填写群组名称", "error"); return; }
    if ((form.members || []).length < 2) { toast("请选择至少两条 WAN 线路", "error"); return; }
    if (form.mode === "weighted") {
      const total = (form.members || []).reduce((sum: number, id: string) => sum + Number(form.weights?.[id] || 0), 0);
      if (total !== 100) { toast("权重合计必须为 100%", "error"); return; }
    }
    if (form.mode === "least_connections" && (form.members || []).some((id: string) => !(Number(form.connections?.[id] || 0) > 0))) {
      toast("最小连接数模式需要为每条线路填写连接数", "error");
      return;
    }
    if (form.mode === "max_idle_bandwidth" && (form.members || []).some((id: string) => !(Number(form.downlink?.[id] || 0) > 0))) {
      toast("最大下行空闲带宽模式需要为每条线路填写下行带宽", "error");
      return;
    }
    setBusy(true);
    try {
      const id = editor?.id || `wangroup-${Date.now().toString(36)}`;
      const payload = {
        id,
        name: form.name.trim(),
        kind: "wan_group",
        direction: "wan",
        load_balance_mode: form.mode,
        mode: form.mode,
        hash_fields: form.hash,
        load_balance: form.hash,
        wan_members: form.members,
        members: form.members,
        weights: form.weights || {},
        member_weights: (form.members || []).map((member: string) => ({ id: member, weight: Number(form.weights?.[member] || 1) })),
        connection_limits: form.connections || {},
        downlink_mbps: form.downlink || {},
        primary_member: form.primary || form.members[0],
        backup_member: form.backup || form.members[1],
      };
      if (editor?.id) await patchJson(API.resources.wanGroup(id), payload);
      else await postJson(API.resources.wanGroups, payload);
      toast("WAN 群组已保存，正在应用", "success");
      setEditor(undefined);
      await queryClient.invalidateQueries({ queryKey: ["resources", "wan-groups"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存 WAN 群组失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (group: WanGroup) => {
    const ok = await confirm({ title: `删除 ${group.name || group.id}？`, message: "引用该群组的策略路由可能失效。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      await deleteJson(API.resources.wanGroup(group.id));
      toast("已删除，正在应用", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources", "wan-groups"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error", error?.requestId);
    }
  };

  const columns = useMemo<ColumnDef<WanGroup, any>[]>(() => [
    { id: "name", header: "群组名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><Waypoints size={14} /><strong>{text(row.original.name || row.original.id)}</strong></span>, meta: { width: 180 } },
    { id: "members", header: "成员线路", accessorFn: (row) => groupMembers(row), cell: ({ row }) => <div className="chip-list">{groupMembers(row.original).map((member) => <span className="chip" key={member}>{wanItems.find((link) => link.id === member)?.name || member}</span>)}</div>, meta: { width: 240 } },
    { id: "mode", header: "负载模式", accessorFn: (row) => row.load_balance_mode || row.mode, cell: ({ getValue }) => text(MODES.find(([key]) => key === getValue())?.[1] || getValue()), meta: { width: 120 } },
    { id: "hash", header: "负载效果", accessorFn: (row) => row.hash_fields || row.load_balance, cell: ({ getValue }) => text(getValue()), meta: { width: 120 } },
    { id: "state", header: "状态", accessorFn: (row) => row.runtime_state, cell: ({ getValue }) => <Badge tone={getValue() === "applied" ? "success" : "warning"} dot>{stateLabel(getValue())}</Badge>, meta: { width: 95 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role, wanItems]);

  return (
    <div className="page">
      <PageHeader
        title="WAN 群组"
        subtitle="多线路负载与主备；成员来自已配置 WAN"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { groups.refetch(); links.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly" || wanItems.length < 2}>新增群组</Button></>}
      />
      <DataTable
        data={groups.data?.items || []}
        columns={columns}
        loading={groups.isLoading}
        error={groups.isError ? groups.error : null}
        onRetry={() => groups.refetch()}
        emptyTitle="暂无 WAN 群组"
        emptyDescription={wanItems.length < 2 ? "至少需要两条 WAN 线路才能创建群组。" : "创建群组以做主备、权重或五元组负载。"}
        emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly" || wanItems.length < 2}>新增群组</Button>}
        getRowId={(row) => row.id}
      />

      <Modal
        open={editor !== undefined}
        onClose={() => !busy && setEditor(undefined)}
        title={editor ? "编辑 WAN 群组" : "新增 WAN 群组"}
        size="lg"
        busy={busy}
        footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}
      >
        <div className="form-grid">
          <Field label="群组名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="负载模式" required>
            <Select value={form.mode || "primary_backup"} onChange={(event) => setForm({ ...form, mode: event.target.value })}>
              {MODES.map(([key, label]) => <option key={key} value={key}>{label}</option>)}
            </Select>
          </Field>
          <Field label="负载效果">
            <Select value={form.hash || "five_tuple"} onChange={(event) => setForm({ ...form, hash: event.target.value })}>
              <option value="five_tuple">五元组</option>
              <option value="src-dst-ip-port">源/目的 IP + 端口</option>
              <option value="src-ip">源 IP</option>
              <option value="dst-ip">目的 IP</option>
            </Select>
          </Field>
          {form.mode === "primary_backup" && <>
            <Field label="主线路"><Select value={form.primary || ""} onChange={(event) => setForm({ ...form, primary: event.target.value })}>{form.members?.map((id: string) => <option key={id} value={id}>{wanItems.find((link) => link.id === id)?.name || id}</option>)}</Select></Field>
            <Field label="备选线路"><Select value={form.backup || ""} onChange={(event) => setForm({ ...form, backup: event.target.value })}>{form.members?.map((id: string) => <option key={id} value={id}>{wanItems.find((link) => link.id === id)?.name || id}</option>)}</Select></Field>
          </>}
          <div className="span-2 member-editor">
            <h3>选择线路</h3>
            {wanItems.map((link) => {
              const checked = (form.members || []).includes(link.id);
              return (
                <div className={`member-row ${checked ? "is-checked" : ""}`} key={link.id}>
                  <label className="member-check">
                    <input type="checkbox" checked={checked} onChange={(event) => setForm({ ...form, members: event.target.checked ? [...(form.members || []), link.id] : (form.members || []).filter((id: string) => id !== link.id) })} />
                    <span><strong>{link.name || link.id}</strong><small>{link.type || link.wan_type || "WAN"} · {link.cidr || "--"}</small></span>
                  </label>
                  {checked && form.mode === "weighted" && <label className="mini-field">权重<input type="number" min={1} max={100} value={form.weights?.[link.id] || 1} onChange={(event) => setForm({ ...form, weights: { ...form.weights, [link.id]: Number(event.target.value) } })} />%</label>}
                  {checked && form.mode === "least_connections" && <label className="mini-field">连接数<input type="number" min={1} value={form.connections?.[link.id] || ""} onChange={(event) => setForm({ ...form, connections: { ...form.connections, [link.id]: Number(event.target.value) } })} /></label>}
                  {checked && form.mode === "max_idle_bandwidth" && <label className="mini-field">下行 M<input type="number" min={1} value={form.downlink?.[link.id] || ""} onChange={(event) => setForm({ ...form, downlink: { ...form.downlink, [link.id]: Number(event.target.value) } })} /></label>}
                </div>
              );
            })}
          </div>
        </div>
      </Modal>
    </div>
  );
}
