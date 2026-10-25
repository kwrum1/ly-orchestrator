import { useEffect, useMemo, useState, type FormEvent } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useNavigate } from "react-router-dom";
import { useQueryClient } from "@tanstack/react-query";
import { Cable, Pencil, Plus, RefreshCw, Router, Trash2, Wifi } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useAudit, useInterfacesResource, usePPPoEStatus, useProxyEgresses, useWanLinks } from "../api/hooks";
import type { InterfaceItem, ProxyEgress, WanLink } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, Select, Switch, Tabs, Textarea, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { auditActionLabel, auditResourceLabel, formatBytes, formatDateTime, formatRate, roleLabel, stateLabel, text, wanTypeLabel } from "../lib/format";
import { sanitizeDetail, takeoverInfo } from "../lib/human";

type EditorState = { kind: "lan" | "wan"; item?: any } | null;

function slug(value: string) {
  return value.toLowerCase().replace(/[^a-z0-9_-]+/g, "-").replace(/^-+|-+$/g, "") || `item-${Date.now()}`;
}

function wanType(item: any) {
  const raw = String(item?.wan_type || item?.type || item?.ipv4?.mode || item?.ipv6?.mode || "").toLowerCase();
  if (raw.includes("pppoe")) return "pppoe";
  if (raw.includes("static6") || raw === "static" && String(item?.cidr || "").includes(":")) return "static6";
  if (raw.includes("dhcp6") || raw.includes("dhcpv6")) return "dhcp6";
  if (raw.includes("dhcp")) return "dhcp4";
  if (raw.includes("proxy") || item?.kind === "egress") return "proxy";
  return "static4";
}

function wanAddress(item: any) {
  return item?.cidr || item?.ipv4?.address || item?.ipv6?.address || "";
}

function wanState(item: any) {
  if (item?.pppoe_state) return item.pppoe_state;
  if (item?.status || item?.state) return item.status || item.state;
  if (item?.runtime_state) return item.runtime_state;
  if (item?.kind === "egress") return item.enabled === false ? "disabled" : "available";
  return item?.enabled === false ? "disabled" : "unknown";
}

function wanHealth(item: any) {
  const state = String(wanState(item)).toLowerCase();
  if (item?.route_ready === true && state === "connected") return "healthy";
  if (["connected", "running", "up", "healthy", "available", "ok"].includes(state)) return "healthy";
  if (["failed", "down", "unavailable"].includes(state)) return "failed";
  return state || "unknown";
}

function pppoePeersOf(data: any) {
  if (Array.isArray(data?.items)) return data.items;
  if (Array.isArray(data?.peers)) return data.peers;
  return [];
}

function handoffLabel(peer: any) {
  const value = peer?.vpp_route_handoff ?? peer?.route_handoff;
  if (typeof value === "string") return value;
  if (value?.state) return value.state;
  if (value?.ready === true || peer?.route_ready === true) return "ready";
  return peer?.route_ready === false ? "pending" : "--";
}

export function WanPage({ mode = "lan" }: { mode?: "lan" | "wan" } = {}) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const interfaces = useInterfacesResource();
  const wanLinks = useWanLinks();
  const proxyEgresses = useProxyEgresses();
  const pppoe = usePPPoEStatus();
  const audit = useAudit();
  const [tab, setTab] = useState<"lan" | "wan" | "logs">(mode);
  useEffect(() => { setTab(mode); }, [mode]);
  const [editor, setEditor] = useState<EditorState>(null);
  const [busy, setBusy] = useState(false);
  const [form, setForm] = useState<any>({});
  const [query, setQuery] = useState("");

  const allInterfaces = interfaces.data?.items || [];
  const lanItems = allInterfaces.filter((item) => (item.gateway_role || item.role) === "lan");
  // A port that failed takeover is not assignable to a line. Management and
  // logical LAN objects are also excluded from the interface selectors.
  const assignableInterfaces = allInterfaces.filter((item) =>
    (item.gateway_role || item.role) !== "management" &&
    takeoverInfo(item).kind !== "none"
  );
  const wanItems = wanLinks.data?.items || [];
  const proxyItems = proxyEgresses.data?.items || [];
  const pppoePeers = pppoePeersOf(pppoe.data);
  const pppoeConnected = pppoePeers.filter((peer: any) => String(peer.state || peer.status || "").toLowerCase() === "connected").length;
  const pppoeSummary = pppoePeers.length ? `${pppoeConnected}/${pppoePeers.length} 已连接` : "未配置";
  const logs = useMemo(() => {
    const payload = audit.data;
    const items = payload?.data?.items || payload?.items || (Array.isArray(payload) ? payload : []);
    return (Array.isArray(items) ? items : []).filter((event: any) => {
      const text = `${event.resource || ""} ${event.action || ""}`.toLowerCase();
      return ["interface", "wan", "proxy", "pppoe"].some((token) => text.includes(token));
    });
  }, [audit.data]);

  const openLan = (item?: any) => {
    setForm({
      name: item?.display_name || item?.name || "",
      kind: item?.kind === "lan_bridge" || item?.type === "lan_bridge" ? "lan_bridge" : "interface",
      interfaceId: item?.interface_id || item?.id || "",
      bridgeMembers: item?.bridge_members || item?.members || [],
      address: String(item?.cidr || item?.address || "").split("/")[0] || "",
      prefix: String(item?.cidr || "").split("/")[1] || "24",
      mtu: item?.mtu || 1500,
      ipv6: item?.ipv6?.mode === "delegated_prefix",
      prefixWan: item?.ipv6?.source_wan_id || "",
      description: item?.description || "",
    });
    setEditor({ kind: "lan", item });
  };

  const openWan = (item?: any) => {
    const type = item ? wanType(item) : "pppoe";
    const address = String(wanAddress(item));
    setForm({
      name: item?.name || "",
      interfaceId: item?.interface_id || item?.underlay_wan_id || "",
      type,
      mtu: item?.mtu || 1492,
      address: address.split("/")[0] || "",
      prefix: address.split("/")[1] || (type === "static6" ? "64" : "24"),
      gateway: item?.gateway || "",
      username: item?.username || "",
      password: "",
      underlay: item?.underlay_wan_id || "",
      proxyAddress: "",
      selection: "fastest",
      topN: 2,
      description: item?.description || "",
    });
    setEditor({ kind: "wan", item });
  };

  const save = async (event?: FormEvent) => {
    event?.preventDefault();
    if (!editor) return;
    if (editor.kind === "lan") {
      const isBridge = form.kind === "lan_bridge";
      if (!form.name?.trim() || !form.address || (!isBridge && !form.interfaceId)) {
        toast("请填写名称、网卡与 IP 地址", "error");
        return;
      }
      if (isBridge && (form.bridgeMembers || []).length < 2) {
        toast("LAN 桥至少需要两张成员网卡", "error");
        return;
      }
      setBusy(true);
      try {
        const id = editor.item?.id || (isBridge ? `lan-bridge-${Date.now().toString(36)}` : form.interfaceId);
        const payload: any = {
          id,
          name: isBridge ? form.name.trim() : form.interfaceId,
          display_name: form.name.trim(),
          interface_id: isBridge ? id : form.interfaceId,
          cidr: `${form.address}/${form.prefix || 24}`,
          gateway: "",
          dns_servers: [],
          nat: false,
          mtu: Number(form.mtu || 1500),
          description: form.description || "",
          gateway_role: "lan",
          mode_role: { gateway: "lan", bridge: isBridge ? form.name.trim() : null },
          candidate_scopes: ["lan"],
          kind: isBridge ? "lan_bridge" : "interface",
          type: isBridge ? "lan_bridge" : "interface",
          bridge_members: isBridge ? form.bridgeMembers : [],
          ipv6: form.ipv6 ? { mode: "delegated_prefix", source_wan_id: form.prefixWan, prefix_delegation: true, ra: true, default_route: true } : { mode: "disabled" },
          runtime_state: "desired_not_applied",
        };
        if (editor.item?.id) await patchJson(API.resources.interface(id), payload);
        else await postJson(API.resources.interfaces, payload);
        toast("LAN 已保存，正在应用", "success");
        setEditor(null);
        await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
        void apply.apply();
      } catch (error: any) {
        toast(error?.message || "保存 LAN 失败", "error");
      } finally {
        setBusy(false);
      }
      return;
    }

    if (!form.name?.trim() || !form.interfaceId) {
      toast("请填写名称与网卡", "error");
      return;
    }
    setBusy(true);
    try {
      const type = form.type;
      const id = editor.item?.id || `wan-${slug(form.name)}-${type}`;
      if (type === "proxy") {
        if (!form.proxyAddress?.trim()) {
          toast("请填写订阅或节点地址", "error");
          setBusy(false);
          return;
        }
        const egressPayload: any = {
          id,
          kind: "egress",
          name: form.name.trim(),
          enabled: true,
          semantic_type: "proxy_egress",
          display_list: "wan",
          proxy_profile_id: "xray-tproxy-outbound",
          runtime_profile: "xray-tproxy-outbound",
          capture_path: "vpp_service_interface",
          engine: "xray",
          handoff: "vpp_to_service",
          listener_mode: "vpp-service",
          underlay_wan_id: form.underlay,
          low_copy: false,
          description: form.description || "",
        };
        if (editor.item?.id) {
          await patchJson(API.resources.proxyEgress(id), egressPayload);
        } else {
          await postJson(API.resources.proxyEgresses, egressPayload);
          const address = form.proxyAddress.trim();
          if (/^https?:\/\//i.test(address)) {
            const subscription = { id: `proxy-sub-${slug(form.name)}`, kind: "subscription", name: `${form.name} 订阅`, enabled: true, url: address, selection: "fastest", top_n: 1 };
            await postJson(API.resources.proxySubscriptions, subscription);
            await patchJson(API.resources.proxyEgress(id), { ...egressPayload, subscription_id: subscription.id });
          } else {
            const protocol = address.split(":", 1)[0].toLowerCase();
            const node = { id: `proxy-node-${slug(form.name)}`, kind: "node", name: `${form.name} 节点`, enabled: true, protocol, uri: address };
            await postJson(API.resources.proxyNodes, node);
            await patchJson(API.resources.proxyEgress(id), { ...egressPayload, node_id: node.id });
          }
        }
      } else {
        const payload: any = {
          id,
          name: form.name.trim(),
          interface_id: form.interfaceId,
          enabled: true,
          gateway_role: "wan",
          mode_role: { gateway: "wan", bridge: null },
          candidate_scopes: ["wan"],
          type: type === "static4" ? "static" : type,
          wan_type: type === "static4" ? "static" : type,
          mtu: Number(form.mtu || 1500),
          description: form.description || "",
          runtime_state: "desired_not_applied",
        };
        if (type === "static4") {
          const cidr = `${form.address}/${form.prefix || 24}`;
          payload.cidr = cidr;
          payload.gateway = form.gateway;
          payload.dns_servers = [];
          payload.nat = true;
          payload.ipv4 = { mode: "static", address: cidr, gateway: form.gateway };
          payload.ipv6 = { mode: "disabled" };
        } else if (type === "static6") {
          const cidr = `${form.address}/${form.prefix || 64}`;
          payload.cidr = cidr;
          payload.gateway = form.gateway;
          payload.dns_servers = [];
          payload.nat = true;
          payload.ipv4 = { mode: "disabled" };
          payload.ipv6 = { mode: "static", address: cidr, gateway: form.gateway };
        } else if (type === "dhcp4" || type === "dhcp6") {
          payload.cidr = "";
          payload.gateway = "";
          payload.dns_servers = [];
          payload.nat = true;
          payload.ipv4 = { mode: type === "dhcp4" ? "dhcp4" : "disabled" };
          payload.ipv6 = { mode: type === "dhcp6" ? "dhcpv6_pd" : "disabled" };
        } else if (type === "pppoe") {
          payload.username = form.username;
          payload.password = form.password;
          payload.ipv4 = { mode: "pppoe" };
        }
        if (editor.item?.id) await patchJson(API.resources.wanLink(id), payload);
        else await postJson(API.resources.wanLinks, payload);
      }
      toast("WAN 已保存，正在应用", "success");
      setEditor(null);
      await queryClient.invalidateQueries({ queryKey: ["resources", "wan-links"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "proxy-egresses"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存 WAN 失败", "error");
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: "lan" | "wan" | "proxy", item: any) => {
    const name = item.name || item.display_name || item.id;
    const ok = await confirm({ title: `删除 ${name}？`, message: "删除会移除该线路的配置，并影响引用它的策略、NAT、DNS 或代理。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      if (kind === "lan") await deleteJson(API.resources.interface(item.id));
      else if (kind === "proxy") await deleteJson(API.resources.proxyEgress(item.id));
      else await deleteJson(API.resources.wanLink(item.id));
      toast("已删除，正在应用", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error");
    }
  };

  const lanColumns = useMemo<ColumnDef<any, any>[]>(() => [
    { id: "name", header: "接口名称", accessorFn: (row) => row.display_name || row.name, cell: ({ row }) => <span className="interface-name"><Cable size={14} /><strong>{text(row.original.display_name || row.original.name || row.original.id)}</strong></span>, meta: { width: 180 } },
    { id: "address", header: "IP / 掩码", accessorFn: (row) => row.cidr || row.address, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    { id: "mtu", header: "MTU", accessorFn: (row) => row.mtu, cell: ({ getValue }) => text(getValue()), meta: { width: 70, align: "right" } },
    { id: "rx", header: "总流入", accessorFn: (row) => row.rx_bytes || row.stats?.rx_bytes || 0, cell: ({ getValue }) => formatBytes(getValue()), meta: { width: 100, align: "right" } },
    { id: "tx", header: "总流出", accessorFn: (row) => row.tx_bytes || row.stats?.tx_bytes || 0, cell: ({ getValue }) => formatBytes(getValue()), meta: { width: 100, align: "right" } },
    { id: "sessions", header: "连接数", accessorFn: (row) => row.sessions || 0, cell: ({ getValue }) => text(getValue(), "0"), meta: { width: 80, align: "right" } },
    { id: "desc", header: "备注", accessorFn: (row) => row.description, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span>, meta: { width: 130 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => openLan(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("lan", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role]);

  const wanColumns = useMemo<ColumnDef<any, any>[]>(() => [
    { id: "name", header: "接口名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><Router size={14} /><strong>{text(row.original.name || row.original.id)}</strong>{row.original.isProxy && <Badge tone="violet">代理</Badge>}{row.original.id === "proxy-egress-default" && <Badge tone="neutral">默认</Badge>}</span>, meta: { width: 190 } },
    { id: "address", header: "当前地址", accessorFn: (row) => wanAddress(row), cell: ({ getValue }) => <span className="mono">{text(getValue(), "--")}</span>, meta: { width: 150 } },
    { id: "state", header: "状态", accessorFn: (row) => wanState(row), cell: ({ row }) => <Badge tone={wanHealth(row.original) === "healthy" ? "success" : wanHealth(row.original) === "failed" ? "danger" : "warning"} dot>{stateLabel(wanState(row.original))}</Badge>, meta: { width: 95 } },
    { id: "type", header: "线路类型", accessorFn: (row) => wanType(row), cell: ({ row }) => <Badge tone="info">{row.original.isProxy ? "代理线路" : wanTypeLabel(wanType(row.original))}</Badge>, meta: { width: 105 } },
    { id: "mtu", header: "MTU", accessorFn: (row) => row.mtu, cell: ({ getValue }) => text(getValue()), meta: { width: 65, align: "right" } },
    { id: "desc", header: "备注", accessorFn: (row) => row.description, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span>, meta: { width: 120 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => row.original.isProxy ? navigate("/proxy") : openWan(row.original)} disabled={session?.role === "readonly"}>编辑</Button>{row.original.id !== "proxy-egress-default" && <Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original.isProxy ? "proxy" : "wan", row.original)} disabled={session?.role === "readonly"}>删除</Button>}</div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role, navigate]);

  const wanRows = [
    ...wanItems.map((item) => ({ ...item, isProxy: false })),
    ...proxyItems.map((item) => ({ ...item, isProxy: true, type: "proxy", wan_type: "proxy" })),
  ];

  const type = form.type;
  return (
    <div className="page">
      <PageHeader
        title={mode === "wan" ? "WAN 线路" : "LAN 线路"}
        subtitle={mode === "wan" ? "WAN、PPPoE 与代理 WAN" : "LAN 接口、IPv4、IPv6、RA 与 DHCPv6"}
        actions={
          <>
            <Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { interfaces.refetch(); wanLinks.refetch(); proxyEgresses.refetch(); pppoe.refetch(); audit.refetch(); }}>刷新</Button>
            <Button size="sm" variant="primary" icon={<Plus size={14} />} disabled={session?.role === "readonly"} onClick={() => tab === "wan" ? openWan() : openLan()}>
              {tab === "wan" ? "新增 WAN" : "新增 LAN"}
            </Button>
          </>
        }
      />

      <div className="status-strip">
        <span><Wifi size={14} /> PPPoE <b>{pppoeSummary}</b></span>
        <span>LAN <b>{lanItems.length}</b></span>
        <span>WAN <b>{wanItems.length}</b></span>
        <span>代理 <b>{proxyItems.length}</b></span>
        <span className="muted">最后更新 {formatDateTime(new Date().toISOString())}</span>
      </div>

      <Tabs
        value={tab}
        onChange={(value) => setTab(value as any)}
        items={[
          { key: "lan", label: "LAN 线路", count: lanItems.length },
          { key: "wan", label: "WAN 线路", count: wanRows.length },
          { key: "logs", label: "线路日志" },
        ]}
      />

      <Card
        title="PPPoE 线路"
        subtitle="PPPoE 连接与地址状态"
        actions={<Badge tone={pppoePeers.length && pppoeConnected === pppoePeers.length ? "success" : "warning"} dot>{pppoePeers.length ? `${pppoeSummary}` : "未配置"}</Badge>}
      >
        {pppoePeers.length ? (
          <div className="mini-table-wrap">
            <table className="data-table">
              <thead><tr><th>Peer</th><th>接口</th><th>地址</th><th>状态</th><th>路由交接</th></tr></thead>
              <tbody>
                {pppoePeers.map((peer: any, index: number) => (
                  <tr key={peer.peer_id || peer.id || index}>
                    <td>{text(peer.peer_id || peer.id || peer.name)}</td>
                    <td className="mono">{text(peer.interface || peer.iface)}</td>
                    <td className="mono">{text(peer.assigned_ipv4 || peer.address, "--")}</td>
                    <td><Badge tone={String(peer.state || peer.status).toLowerCase() === "connected" ? "success" : "warning"}>{stateLabel(peer.state || peer.status)}</Badge></td>
                    <td><span className="mono">{stateLabel(handoffLabel(peer))}</span>{peer.route_ready === true && <span className="muted"> · ready</span>}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : <p className="muted">未配置 PPPoE</p>}
      </Card>

      {tab === "lan" && (
        <DataTable
          data={lanItems.filter((item) => !query || JSON.stringify(item).toLowerCase().includes(query.toLowerCase()))}
          columns={lanColumns}
          loading={interfaces.isLoading}
          error={interfaces.isError ? interfaces.error : null}
          onRetry={() => interfaces.refetch()}
          emptyTitle="暂无 LAN 接口"
          emptyDescription="先在网卡设置中把接口标记为 LAN，再创建 LAN 接口。"
          emptyAction={<Button size="sm" variant="primary" onClick={() => openLan()} disabled={session?.role === "readonly"}>新增 LAN</Button>}
          getRowId={(row) => row.id}
        />
      )}
      {tab === "wan" && (
        <DataTable
          data={wanRows}
          columns={wanColumns}
          loading={wanLinks.isLoading || proxyEgresses.isLoading}
          error={wanLinks.isError && !wanRows.length ? wanLinks.error : null}
          onRetry={() => { wanLinks.refetch(); proxyEgresses.refetch(); }}
          emptyTitle="暂无 WAN 线路"
          emptyDescription="创建 PPPoE、静态、DHCP 或代理线路后，策略路由与 NAT 才能选择出口。"
          emptyAction={<Button size="sm" variant="primary" onClick={() => openWan()} disabled={session?.role === "readonly"}>新增 WAN</Button>}
          getRowId={(row) => row.id}
        />
      )}
      {tab === "logs" && (
        <DataTable
          data={logs}
          columns={[
            { id: "time", header: "时间", accessorFn: (row) => row.timestamp, cell: ({ getValue }) => <span className="muted">{formatDateTime(getValue())}</span>, meta: { width: 170 } },
            { id: "actor", header: "操作者", accessorFn: (row) => row.actor, cell: ({ getValue }) => text(getValue()), meta: { width: 110 } },
            { id: "resource", header: "对象", accessorFn: (row) => row.resource, cell: ({ getValue }) => <span>{auditResourceLabel(getValue())}</span>, meta: { width: 260 } },
            { id: "action", header: "动作", accessorFn: (row) => row.action, cell: ({ getValue }) => auditActionLabel(getValue()), meta: { width: 100 } },
            { id: "status", header: "结果", accessorFn: (row) => row.status, cell: ({ getValue }) => <Badge tone={String(getValue()) === "success" ? "success" : "danger"}>{String(getValue()) === "success" ? "成功" : "失败"}</Badge>, meta: { width: 90 } },
            { id: "error", header: "说明", accessorFn: (row) => row.error, cell: ({ getValue }) => <span className="muted">{sanitizeDetail(getValue(), "")}</span> },
          ]}
          loading={audit.isLoading}
          error={audit.isError ? audit.error : null}
          onRetry={() => audit.refetch()}
          emptyTitle="暂无线路日志"
          emptyDescription="接口、WAN、代理的审计事件会显示在这里。"
          getRowId={(row, index) => row.id || `${row.timestamp}-${index}`}
        />
      )}

      <Modal
        open={Boolean(editor)}
        onClose={() => !busy && setEditor(null)}
        title={editor?.kind === "wan" ? (editor?.item ? "编辑 WAN" : "新增 WAN") : (editor?.item ? "编辑 LAN" : "新增 LAN")}
        subtitle={editor?.kind === "wan" ? "保存后立即应用" : "接口角色必须已在网卡设置中标记为 LAN"}
        size={editor?.kind === "wan" ? "lg" : "md"}
        busy={busy}
        footer={<><Button onClick={() => setEditor(null)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={() => save()}>保存并应用</Button></>}
      >
        {editor?.kind === "lan" ? (
          <div className="form-grid">
            <Field label="名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="lan-lab" /></Field>
            <Field label="接口类型"><Select value={form.kind || "interface"} onChange={(event) => setForm({ ...form, kind: event.target.value })} disabled={Boolean(editor?.item?.id)}><option value="interface">LAN 接口</option><option value="lan_bridge">LAN 桥</option></Select></Field>
            {form.kind === "lan_bridge" ? (
              <Field label="LAN 桥成员" required className="span-2" hint="选择至少两张 LAN 网卡；保存后作为逻辑桥接口">
                <div className="check-grid">
                  {assignableInterfaces.map((item) => (
                    <label className="check-card" key={item.id}>
                      <input
                        type="checkbox"
                        checked={(form.bridgeMembers || []).includes(item.id)}
                        onChange={(event) => setForm({
                          ...form,
                          bridgeMembers: event.target.checked
                            ? [...(form.bridgeMembers || []), item.id]
                            : (form.bridgeMembers || []).filter((id: string) => id !== item.id),
                        })}
                      />
                      <span><strong>{item.name || item.id}</strong><small>{item.speed || "--"} · {item.work_mode || "--"}</small></span>
                    </label>
                  ))}
                </div>
              </Field>
            ) : (
              <Field label="网卡" required>
                <Select value={form.interfaceId || ""} onChange={(event) => setForm({ ...form, interfaceId: event.target.value })}>
                  <option value="">请选择 LAN 网卡</option>
                  {assignableInterfaces.map((item) => <option key={item.id} value={item.id}>{item.name || item.id} · {item.speed || "--"}</option>)}
                </Select>
              </Field>
            )}
            <Field label="IP 地址" required><Input value={form.address || ""} onChange={(event) => setForm({ ...form, address: event.target.value })} placeholder="192.168.88.1" /></Field>
            <Field label="前缀" required><Input type="number" min={1} max={32} value={form.prefix || "24"} onChange={(event) => setForm({ ...form, prefix: event.target.value })} /></Field>
            <Field label="MTU"><Input type="number" value={form.mtu || 1500} onChange={(event) => setForm({ ...form, mtu: event.target.value })} /></Field>
            <Field label="备注"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
            <Field label="IPv6 前缀派发" className="span-2"><Switch checked={Boolean(form.ipv6)} onChange={(value) => setForm({ ...form, ipv6: value })} label="启用 IPv6 前缀派发（来自 WAN）" /></Field>
            {form.ipv6 && <Field label="前缀来源 WAN" className="span-2"><Select value={form.prefixWan || ""} onChange={(event) => setForm({ ...form, prefixWan: event.target.value })}><option value="">不指定</option>{wanItems.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field>}
          </div>
        ) : (
          <div className="form-grid">
            <Field label="名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="wan-main" /></Field>
            <Field label="网卡" required>
              <Select value={form.interfaceId || ""} onChange={(event) => setForm({ ...form, interfaceId: event.target.value })}>
                <option value="">请选择 WAN 网卡</option>
                {assignableInterfaces.map((item) => <option key={item.id} value={item.id}>{item.name || item.id} · {item.speed || "--"}</option>)}
              </Select>
            </Field>
            <Field label="线路类型" required>
              <Select value={type || "pppoe"} onChange={(event) => setForm({ ...form, type: event.target.value })} disabled={Boolean(editor?.item?.id)}>
                <option value="pppoe">PPPoE</option>
                <option value="static4">固定 IPv4</option>
                <option value="static6">固定 IPv6</option>
                <option value="dhcp4">DHCP IPv4</option>
                <option value="dhcp6">DHCPv6-PD</option>
                <option value="proxy">代理线路</option>
              </Select>
            </Field>
            <Field label="MTU"><Input type="number" value={form.mtu || 1500} onChange={(event) => setForm({ ...form, mtu: event.target.value })} /></Field>
            {type === "pppoe" && <>
              <Field label="账号" required><Input value={form.username || ""} onChange={(event) => setForm({ ...form, username: event.target.value })} /></Field>
              <Field label="密码" required><Input type="password" value={form.password || ""} onChange={(event) => setForm({ ...form, password: event.target.value })} placeholder={editor?.item ? "已保存（不显示）" : ""} /></Field>
            </>}
            {(type === "static4" || type === "static6") && <>
              <Field label={type === "static4" ? "IPv4 地址" : "IPv6 地址"} required><Input value={form.address || ""} onChange={(event) => setForm({ ...form, address: event.target.value })} placeholder={type === "static4" ? "203.0.113.18" : "2001:db8::2"} /></Field>
              <Field label="前缀" required><Input type="number" value={form.prefix || (type === "static4" ? "24" : "64")} onChange={(event) => setForm({ ...form, prefix: event.target.value })} /></Field>
              <Field label="网关" required><Input value={form.gateway || ""} onChange={(event) => setForm({ ...form, gateway: event.target.value })} /></Field>
            </>}
            {type === "proxy" && <>
              <Field label="承载出口" required><Select value={form.underlay || ""} onChange={(event) => setForm({ ...form, underlay: event.target.value })}><option value="">请选择 WAN</option>{wanItems.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field>
              <Field label="订阅/节点地址" required className="span-2"><Textarea rows={3} value={form.proxyAddress || ""} onChange={(event) => setForm({ ...form, proxyAddress: event.target.value })} placeholder="https://example.com/sub 或 vless://..." /></Field>
              <Field label="节点选择"><Select value={form.selection || "fastest"} onChange={(event) => setForm({ ...form, selection: event.target.value })}><option value="fastest">自动选择线路</option><option value="adaptive">最小延迟（加权）</option></Select></Field>
              <Field label="参与节点数"><Select value={String(form.topN || 2)} onChange={(event) => setForm({ ...form, topN: Number(event.target.value) })}><option value="2">Top 2</option><option value="3">Top 3</option></Select></Field>
            </>}
            <Field label="备注" className="span-2"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
          </div>
        )}
      </Modal>
    </div>
  );
}
