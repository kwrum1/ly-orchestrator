import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RefreshCw, Trash2, Wifi } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useDhcpBindings, useDhcpLeases, useDhcpServers, useInterfacesResource, useRuntime } from "../api/hooks";
import type { DhcpBinding, DhcpLease, DhcpServer } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, Tabs, Textarea, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { formatDateTime, splitLines, stateLabel, text } from "../lib/format";
import { takeoverInfo } from "../lib/human";

function leaseLabel(seconds: unknown) {
  const value = Number(seconds);
  if (!Number.isFinite(value) || value <= 0) return "--";
  if (value % 3600 === 0) return `${value / 3600} 小时`;
  return `${Math.round(value / 60)} 分钟`;
}

function dhcpRuntimeState(item: any, runtime: any, applying: boolean, failed: boolean) {
  const raw = item?.runtime_state || (item?.enabled === false ? "disabled" : "enabled");
  if (raw !== "desired_not_applied" || applying || failed) return raw;
  const last = runtime?.last_apply;
  const kea = (runtime?.components || []).find((component: any) => component.name === "dnsmasq");
  if (String(last?.status || "").toLowerCase() === "committed" && (kea?.state === "running" || kea?.available === true)) return "applied";
  return raw;
}

export function DhcpPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const servers = useDhcpServers();
  const bindings = useDhcpBindings();
  const leases = useDhcpLeases();
  const interfaces = useInterfacesResource();
  const runtime = useRuntime();
  const [tab, setTab] = useState<"servers" | "bindings" | "leases">("servers");
  const [editor, setEditor] = useState<DhcpServer | DhcpBinding | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const lanInterfaces = (interfaces.data?.items || []).filter((item) => (item.gateway_role || item.role) === "lan");
  const assignable = (item: any) => {
    const kind = String(item?.kind || item?.type || "").toLowerCase();
    if (kind === "lan_bridge" || kind === "bridge") return true;
    if ((item?.gateway_role || item?.role) === "management") return false;
    return takeoverInfo(item).assignable;
  };
  const candidates = (lanInterfaces.length ? lanInterfaces : (interfaces.data?.items || []).filter((item) => (item.gateway_role || item.role) !== "management"))
    .filter(assignable);

  const openServer = (item?: DhcpServer) => {
    setForm({
      name: item?.name || "",
      interfaceId: item?.interface_id || "",
      subnet: item?.subnet || "",
      pools: (item?.pools || []).join("\n"),
      routers: (item?.routers || []).join(", "),
      lease: item?.lease_time_seconds ? Math.round(Number(item.lease_time_seconds) / 3600) : 12,
      description: item?.description || "",
    });
    setEditor(item || null);
  };

  const openBinding = (item?: DhcpBinding) => {
    setForm({
      mac: item?.mac || "",
      ip: item?.ip || "",
      hostname: item?.hostname || "",
      description: item?.description || "",
    });
    setEditor(item || null);
  };

  const save = async () => {
    if (tab === "servers") {
      if (!form.interfaceId || !form.name?.trim()) { toast("请填写名称与 LAN 接口", "error"); return; }
      setBusy(true);
      try {
        const id = (editor as DhcpServer)?.id || `dhcp-${form.interfaceId}`;
        const payload = {
          id,
          name: form.name.trim(),
          description: form.description || "",
          engine: "dnsmasq",
          enabled: true,
          interface_id: form.interfaceId,
          subnet: form.subnet || "",
          pools: splitLines(form.pools || ""),
          routers: String(form.routers || "").split(",").map((item) => item.trim()).filter(Boolean),
          name_servers: [],
          lease_time_seconds: Number(form.lease || 12) * 3600,
        };
        if ((editor as DhcpServer)?.id) await patchJson(API.resources.dhcpServer(id), payload);
        else await postJson(API.resources.dhcpServers, payload);
        await queryClient.invalidateQueries({ queryKey: ["resources", "dhcp-servers"] });
        toast("DHCP 服务已保存，正在应用", "success");
        setEditor(undefined);
        void apply.apply();
      } catch (error: any) {
        toast(error?.message || "保存 DHCP 服务失败", "error");
      } finally {
        setBusy(false);
      }
      return;
    }
    if (!form.mac?.trim() || !form.ip?.trim()) { toast("请填写 MAC 与 IP", "error"); return; }
    setBusy(true);
    try {
      const id = (editor as DhcpBinding)?.id || `dhcp-binding-${form.mac.replace(/[^a-zA-Z0-9]/g, "")}`;
      const payload = { id, mac: form.mac.trim(), ip: form.ip.trim(), hostname: form.hostname || "", description: form.description || "", enabled: true };
      if ((editor as DhcpBinding)?.id) await patchJson(API.resources.dhcpBinding(id), payload);
      else await postJson(API.resources.dhcpBindings, payload);
      await queryClient.invalidateQueries({ queryKey: ["resources", "dhcp-bindings"] });
      toast("静态分配已保存，正在应用", "success");
      setEditor(undefined);
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存静态分配失败", "error");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: "server" | "binding", item: any) => {
    const ok = await confirm({ title: `删除 ${item.name || item.mac || item.id}？`, message: "删除后客户端可能无法获取租约或固定地址。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      if (kind === "server") await deleteJson(API.resources.dhcpServer(item.id));
      else await deleteJson(API.resources.dhcpBinding(item.id));
      toast("已删除，正在应用", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources", kind === "server" ? "dhcp-servers" : "dhcp-bindings"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error");
    }
  };

  const serverColumns = useMemo<ColumnDef<DhcpServer, any>[]>(() => [
    { id: "name", header: "服务名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><Wifi size={14} /><strong>{text(row.original.name || row.original.id)}</strong></span>, meta: { width: 180 } },
    { id: "interface", header: "LAN 接口", accessorFn: (row) => row.interface_id, cell: ({ getValue }) => text(getValue()), meta: { width: 130 } },
    { id: "subnet", header: "子网", accessorFn: (row) => row.subnet, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    { id: "pools", header: "地址池", accessorFn: (row) => row.pools, cell: ({ getValue }) => <span className="mono">{(getValue() || []).join(", ") || "--"}</span>, meta: { width: 190 } },
    { id: "gateway", header: "网关", accessorFn: (row) => row.routers?.[0], cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 130 } },
    { id: "lease", header: "租约", accessorFn: (row) => row.lease_time_seconds, cell: ({ getValue }) => leaseLabel(getValue()), meta: { width: 90 } },
    { id: "state", header: "状态", accessorFn: (row) => dhcpRuntimeState(row, runtime.data, apply.running, apply.phase === "failed"), cell: ({ getValue }) => <Badge tone={getValue() === "applied" ? "success" : getValue() === "disabled" ? "neutral" : "warning"} dot>{stateLabel(getValue())}</Badge>, meta: { width: 90 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openServer(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("server", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role, runtime.data, apply.running, apply.phase]);

  const bindingColumns = useMemo<ColumnDef<DhcpBinding, any>[]>(() => [
    { id: "mac", header: "MAC", accessorFn: (row) => row.mac, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 170, mono: true } },
    { id: "ip", header: "IP", accessorFn: (row) => row.ip, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150, mono: true } },
    { id: "hostname", header: "主机名", accessorFn: (row) => row.hostname, cell: ({ getValue }) => text(getValue(), "--"), meta: { width: 160 } },
    { id: "desc", header: "备注", accessorFn: (row) => row.description, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span> },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openBinding(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("binding", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role]);

  const leaseColumns = useMemo<ColumnDef<DhcpLease, any>[]>(() => [
    { id: "ip", header: "IP 地址", accessorFn: (row) => row.ip_address, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150, mono: true } },
    { id: "mac", header: "MAC", accessorFn: (row) => row.mac, cell: ({ getValue }) => <span className="mono">{text(getValue(), "--")}</span>, meta: { width: 170, mono: true } },
    { id: "hostname", header: "主机名", accessorFn: (row) => row.hostname, cell: ({ getValue }) => text(getValue(), "--"), meta: { width: 160 } },
    { id: "expires", header: "到期", accessorFn: (row) => row.infinite ? "永久" : formatDateTime(row.lease_end), cell: ({ getValue }) => text(getValue(), "--"), meta: { width: 180 } },
    { id: "state", header: "状态", accessorFn: (row) => row.state, cell: ({ getValue }) => <Badge tone={getValue() === "active" ? "success" : "warning"} dot>{stateLabel(getValue())}</Badge>, meta: { width: 90 } },
  ], []);

  return (
    <div className="page">
      <PageHeader
        title="DHCP 服务"
        subtitle="动态地址池、静态分配与租约；服务依赖 LAN 接口"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { servers.refetch(); bindings.refetch(); leases.refetch(); interfaces.refetch(); }}>刷新</Button>{tab !== "leases" && <Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => tab === "servers" ? openServer() : openBinding()} disabled={session?.role === "readonly"}>{tab === "servers" ? "新增服务" : "新增静态分配"}</Button>}</>}
      />
      <Tabs value={tab} onChange={(value) => setTab(value as any)} items={[{ key: "servers", label: "服务列表", count: servers.data?.items?.length }, { key: "bindings", label: "静态分配", count: bindings.data?.items?.length }, { key: "leases", label: "租约", count: leases.data?.items?.length }]} />
      {tab === "servers" ? (
        <DataTable data={servers.data?.items || []} columns={serverColumns} loading={servers.isLoading} error={servers.isError ? servers.error : null} onRetry={() => servers.refetch()} emptyTitle="暂无 DHCP 服务" emptyDescription={candidates.length ? "为 LAN 接口创建 DHCP 服务。" : "请先创建 LAN 接口。"} emptyAction={<Button size="sm" variant="primary" onClick={() => openServer()} disabled={session?.role === "readonly" || !candidates.length}>新增服务</Button>} getRowId={(row) => row.id} />
      ) : tab === "bindings" ? (
        <DataTable data={bindings.data?.items || []} columns={bindingColumns} loading={bindings.isLoading} error={bindings.isError ? bindings.error : null} onRetry={() => bindings.refetch()} emptyTitle="暂无静态分配" emptyDescription="将 MAC 固定到指定 IP。" emptyAction={<Button size="sm" variant="primary" onClick={() => openBinding()} disabled={session?.role === "readonly"}>新增静态分配</Button>} getRowId={(row) => row.id} />
      ) : (
        <DataTable data={leases.data?.items || []} columns={leaseColumns} loading={leases.isLoading} error={leases.isError ? leases.error : null} onRetry={() => leases.refetch()} emptyTitle="暂无租约" emptyDescription="终端从本机取得地址后会显示在这里。" getRowId={(row) => row.id} />
      )}

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={tab === "servers" ? (editor ? "编辑 DHCP 服务" : "新增 DHCP 服务") : (editor ? "编辑静态分配" : "新增静态分配")} size="md" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}>
        {tab === "servers" ? (
          <div className="form-grid">
            <Field label="服务名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
            <Field label="LAN 接口" required><Select value={form.interfaceId || ""} onChange={(event) => setForm({ ...form, interfaceId: event.target.value })}><option value="">请选择接口</option>{candidates.map((item) => <option key={item.id} value={item.id}>{item.display_name || item.name || item.id}</option>)}</Select></Field>
            <Field label="子网" hint="留空由后端按 LAN 推导"><Input value={form.subnet || ""} onChange={(event) => setForm({ ...form, subnet: event.target.value })} placeholder="192.168.88.0/24" /></Field>
            <Field label="租约（小时）"><Input type="number" min={1} value={form.lease || 12} onChange={(event) => setForm({ ...form, lease: event.target.value })} /></Field>
            <Field label="地址池" className="span-2" hint="每行一个范围，例如 192.168.88.100-192.168.88.199"><Textarea rows={4} value={form.pools || ""} onChange={(event) => setForm({ ...form, pools: event.target.value })} /></Field>
            <Field label="网关" className="span-2"><Input value={form.routers || ""} onChange={(event) => setForm({ ...form, routers: event.target.value })} placeholder="192.168.88.1" /></Field>
            <Field label="备注" className="span-2"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
          </div>
        ) : (
          <div className="form-grid">
            <Field label="终端 MAC" required><Input value={form.mac || ""} onChange={(event) => setForm({ ...form, mac: event.target.value })} placeholder="aa:bb:cc:dd:ee:ff" /></Field>
            <Field label="终端 IP" required><Input value={form.ip || ""} onChange={(event) => setForm({ ...form, ip: event.target.value })} placeholder="192.168.88.50" /></Field>
            <Field label="主机名"><Input value={form.hostname || ""} onChange={(event) => setForm({ ...form, hostname: event.target.value })} /></Field>
            <Field label="备注"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
          </div>
        )}
      </Modal>
    </div>
  );
}
