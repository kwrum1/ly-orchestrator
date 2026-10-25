import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftRight, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { usePortMaps, useStaticNat, useWanGroups, useWanLinks } from "../api/hooks";
import type { PortMap, StaticNat } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, Switch, Tabs, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { formatDateTime, stateLabel, text } from "../lib/format";

type Tab = "port" | "static";

export function PortMappingPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const portMaps = usePortMaps();
  const staticNat = useStaticNat();
  const links = useWanLinks();
  const groups = useWanGroups();
  const [tab, setTab] = useState<Tab>("port");
  const [editor, setEditor] = useState<PortMap | StaticNat | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const wanOptions = [
    ...(links.data?.items || []).map((item) => ({ id: item.id, label: item.name || item.id })),
    ...(groups.data?.items || []).map((item) => ({ id: item.id, label: `WAN组 · ${item.name || item.id}` })),
  ];

  const openPort = (item?: PortMap) => {
    setForm({
      name: item?.name || "",
      wanLink: item?.wan_link || "",
      protocol: item?.protocol || "TCP",
      externalAddress: item?.external_address || "",
      externalPort: item?.external_port || "",
      internalHost: item?.internal_host || "",
      internalPort: item?.internal_port || "",
      description: item?.description || "",
      hairpin: Boolean(item?.hairpin),
      fullCone: Boolean(item?.full_cone),
    });
    setEditor(item || null);
  };

  const openStatic = (item?: StaticNat) => {
    setForm({
      name: item?.name || "",
      wanLink: item?.wan_link || item?.wan_interface || "",
      externalAddress: item?.external_address || "",
      internalAddress: item?.internal_address || "",
      description: item?.description || "",
    });
    setEditor(item || null);
  };

  const save = async () => {
    if (!form.name?.trim()) { toast("请填写名称", "error"); return; }
    setBusy(true);
    try {
      if (tab === "port") {
        if (!form.wanLink || !form.externalPort || !form.internalHost || !form.internalPort) {
          toast("请填写线路、外部端口、内网主机和内网端口", "error");
          setBusy(false);
          return;
        }
        const id = (editor as PortMap)?.id || `port-map-${Date.now().toString(36)}`;
        const payload = {
          id,
          name: form.name.trim(),
          wan_link: form.wanLink,
          protocol: String(form.protocol || "TCP").toUpperCase(),
          external_address: form.externalAddress || "",
          external_port: Number(form.externalPort),
          internal_host: form.internalHost,
          internal_port: Number(form.internalPort),
          description: form.description || "",
          hairpin: Boolean(form.hairpin),
          full_cone: Boolean(form.fullCone),
          enabled: true,
        };
        if ((editor as PortMap)?.id) await patchJson(API.resources.portMap(id), payload);
        else await postJson(API.resources.portMaps, payload);
        await queryClient.invalidateQueries({ queryKey: ["resources", "port-maps"] });
      } else {
        if (!form.wanLink || !form.externalAddress || !form.internalAddress) {
          toast("请填写线路、外部地址和内网地址", "error");
          setBusy(false);
          return;
        }
        const id = (editor as StaticNat)?.id || `nat-static-${Date.now().toString(36)}`;
        const payload = {
          id,
          name: form.name.trim(),
          wan_link: form.wanLink,
          wan_interface: form.wanLink,
          external_address: form.externalAddress,
          internal_address: form.internalAddress,
          description: form.description || "",
          enabled: true,
        };
        if ((editor as StaticNat)?.id) await patchJson(API.resources.staticNatItem(id), payload);
        else await postJson(API.resources.staticNat, payload);
        await queryClient.invalidateQueries({ queryKey: ["resources", "static-nat"] });
      }
      toast("NAT 规则已保存，正在应用", "success");
      setEditor(undefined);
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存 NAT 规则失败", "error");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: Tab, item: any) => {
    const ok = await confirm({ title: `删除 ${item.name || item.id}？`, message: "删除会立即影响映射流量。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      if (kind === "port") await deleteJson(API.resources.portMap(item.id));
      else await deleteJson(API.resources.staticNatItem(item.id));
      toast("已删除，正在应用", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources", kind === "port" ? "port-maps" : "static-nat"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error");
    }
  };

  const portColumns = useMemo<ColumnDef<PortMap, any>[]>(() => [
    { id: "name", header: "策略名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><ArrowLeftRight size={14} /><strong>{text(row.original.name || row.original.id)}</strong></span>, meta: { width: 190 } },
    { id: "wan", header: "映射线路", accessorFn: (row) => row.wan_link, cell: ({ getValue }) => <span>{wanOptions.find((item) => item.id === getValue())?.label || text(getValue(), "--")}</span>, meta: { width: 150 } },
    { id: "external", header: "外部", accessorFn: (row) => `${row.external_address || "*"}:${row.external_port || "-"}`, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    { id: "internal", header: "内网", accessorFn: (row) => `${row.internal_host || "-"}:${row.internal_port || "-"}`, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 170 } },
    { id: "protocol", header: "协议", accessorFn: (row) => row.protocol, cell: ({ getValue }) => <Badge tone="info">{text(getValue(), "TCP").toUpperCase()}</Badge>, meta: { width: 80 } },
    { id: "sessions", header: "会话", accessorFn: (row) => row.session_count, cell: ({ getValue }) => text(getValue(), "--"), meta: { width: 70, align: "right" } },
    { id: "hit", header: "最近命中", accessorFn: (row) => row.last_hit_at, cell: ({ getValue }) => <span className="muted">{formatDateTime(getValue())}</span>, meta: { width: 150 } },
    { id: "state", header: "状态", accessorFn: (row) => row.runtime_state, cell: ({ row }) => <Badge tone={row.original.runtime_state === "applied" ? "success" : "warning"} dot>{stateLabel(row.original.runtime_state)}</Badge>, meta: { width: 90 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openPort(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("port", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [wanOptions, session?.role]);

  const staticColumns = useMemo<ColumnDef<StaticNat, any>[]>(() => [
    { id: "name", header: "名称", accessorFn: (row) => row.name, cell: ({ row }) => <strong>{text(row.original.name || row.original.id)}</strong>, meta: { width: 180 } },
    { id: "wan", header: "线路", accessorFn: (row) => row.wan_link || row.wan_interface, cell: ({ getValue }) => <span>{wanOptions.find((item) => item.id === getValue())?.label || text(getValue(), "--")}</span>, meta: { width: 150 } },
    { id: "external", header: "外部地址", accessorFn: (row) => row.external_address, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    { id: "internal", header: "内网地址", accessorFn: (row) => row.internal_address, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    { id: "desc", header: "备注", accessorFn: (row) => row.description, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span> },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openStatic(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("static", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [wanOptions, session?.role]);

  return (
    <div className="page">
      <PageHeader
        title="端口映射"
        subtitle="入站端口映射与一对一静态 NAT"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { portMaps.refetch(); staticNat.refetch(); links.refetch(); groups.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => tab === "port" ? openPort() : openStatic()} disabled={session?.role === "readonly"}>{tab === "port" ? "新增映射" : "新增静态 NAT"}</Button></>}
      />
      <Tabs value={tab} onChange={(value) => setTab(value as Tab)} items={[{ key: "port", label: "端口映射", count: portMaps.data?.items?.length }, { key: "static", label: "静态 NAT", count: staticNat.data?.items?.length }]} />
      {tab === "port" ? (
        <DataTable data={portMaps.data?.items || []} columns={portColumns} loading={portMaps.isLoading} error={portMaps.isError ? portMaps.error : null} onRetry={() => portMaps.refetch()} emptyTitle="暂无端口映射" emptyDescription="创建映射后，外部端口会转发到内网主机。" emptyAction={<Button size="sm" variant="primary" onClick={() => openPort()} disabled={session?.role === "readonly"}>新增映射</Button>} getRowId={(row) => row.id} />
      ) : (
        <DataTable data={staticNat.data?.items || []} columns={staticColumns} loading={staticNat.isLoading} error={staticNat.isError ? staticNat.error : null} onRetry={() => staticNat.refetch()} emptyTitle="暂无静态 NAT" emptyDescription="静态 NAT 仅支持 IPv4 NAT44。" emptyAction={<Button size="sm" variant="primary" onClick={() => openStatic()} disabled={session?.role === "readonly"}>新增静态 NAT</Button>} getRowId={(row) => row.id} />
      )}

      <Modal
        open={editor !== undefined}
        onClose={() => !busy && setEditor(undefined)}
        title={tab === "port" ? (editor ? "编辑端口映射" : "新增端口映射") : (editor ? "编辑静态 NAT" : "新增静态 NAT")}
        size="md"
        busy={busy}
        footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}
      >
        {tab === "port" ? (
          <div className="form-grid">
            <Field label="规则名" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
            <Field label="映射线路" required><Select value={form.wanLink || ""} onChange={(event) => setForm({ ...form, wanLink: event.target.value })}><option value="">请选择</option>{wanOptions.map((item) => <option key={item.id} value={item.id}>{item.label}</option>)}</Select></Field>
            <Field label="协议"><Select value={form.protocol || "TCP"} onChange={(event) => setForm({ ...form, protocol: event.target.value })}><option value="TCP">TCP</option><option value="UDP">UDP</option><option value="TCP_UDP">TCP+UDP</option></Select></Field>
            <Field label="外部端口" required><Input type="number" min={1} max={65535} value={form.externalPort || ""} onChange={(event) => setForm({ ...form, externalPort: event.target.value })} /></Field>
            <Field label="内网主机" required><Input value={form.internalHost || ""} onChange={(event) => setForm({ ...form, internalHost: event.target.value })} placeholder="192.168.88.10" /></Field>
            <Field label="内网端口" required><Input type="number" min={1} max={65535} value={form.internalPort || ""} onChange={(event) => setForm({ ...form, internalPort: event.target.value })} /></Field>
            <Field label="外部地址" hint="留空使用 WAN 线路地址"><Input value={form.externalAddress || ""} onChange={(event) => setForm({ ...form, externalAddress: event.target.value })} /></Field>
            <Field label="备注"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
            <Field label="高级"><div className="inline-switches"><Switch checked={Boolean(form.hairpin)} onChange={(value) => setForm({ ...form, hairpin: value })} label="回流" /><Switch checked={Boolean(form.fullCone)} onChange={(value) => setForm({ ...form, fullCone: value })} label="全锥" /></div></Field>
          </div>
        ) : (
          <div className="form-grid">
            <Field label="名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
            <Field label="映射线路" required><Select value={form.wanLink || ""} onChange={(event) => setForm({ ...form, wanLink: event.target.value })}><option value="">请选择</option>{wanOptions.map((item) => <option key={item.id} value={item.id}>{item.label}</option>)}</Select></Field>
            <Field label="外部地址" required><Input value={form.externalAddress || ""} onChange={(event) => setForm({ ...form, externalAddress: event.target.value })} placeholder="203.0.113.10" /></Field>
            <Field label="内网地址" required><Input value={form.internalAddress || ""} onChange={(event) => setForm({ ...form, internalAddress: event.target.value })} placeholder="192.168.88.10" /></Field>
            <Field label="备注" className="span-2"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
          </div>
        )}
      </Modal>
    </div>
  );
}
