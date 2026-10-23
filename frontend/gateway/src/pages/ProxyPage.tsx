import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { CloudCog, Pencil, Plus, RefreshCw, RotateCw, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useInterfacesResource, useProxyEgresses, useProxyLogs, useProxyNodes, useProxyStatus, useProxySubscriptions, useWanLinks } from "../api/hooks";
import type { ProxyEgress, ProxyNode, ProxySubscription } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, Select, Tabs, Textarea, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { stateLabel, text } from "../lib/format";

type Tab = "egress" | "nodes" | "subscriptions";

export function ProxyPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const egresses = useProxyEgresses();
  const nodes = useProxyNodes();
  const subscriptions = useProxySubscriptions();
  const links = useWanLinks();
  const interfaces = useInterfacesResource();
  const status = useProxyStatus();
  const logs = useProxyLogs();
  const [tab, setTab] = useState<Tab>("egress");
  const [editor, setEditor] = useState<any | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const underlays = links.data?.items || [];
  const egressItems = egresses.data?.items || [];
  const nodeItems = nodes.data?.items || [];
  const subscriptionItems = subscriptions.data?.items || [];

  const open = (kind: Tab, item?: any) => {
    setForm({
      name: item?.name || "",
      underlay: item?.underlay_wan_id || "",
      node: item?.node_id || "",
      subscription: item?.subscription_id || "",
      description: item?.description || "",
      protocol: item?.protocol || "vless",
      uri: item?.uri || "",
      url: item?.url || "",
      selection: item?.selection || "fastest",
      topN: item?.top_n || 2,
    });
    setEditor({ kind, item: item || null });
  };

  const save = async () => {
    if (!form.name?.trim()) { toast("请填写名称", "error"); return; }
    setBusy(true);
    try {
      const kind = editor?.kind;
      if (kind === "egress") {
        const id = editor?.item?.id || `proxy-egress-${Date.now().toString(36)}`;
        const payload: any = {
          id, kind: "egress", name: form.name.trim(), enabled: true, semantic_type: "proxy_egress", display_list: "wan",
          proxy_profile_id: "xray-tproxy-outbound", runtime_profile: "xray-tproxy-outbound", capture_path: "vpp_service_interface",
          engine: "xray", handoff: "vpp_to_service", listener_mode: "vpp-service", underlay_wan_id: form.underlay, low_copy: false,
          description: form.description || "",
        };
        if (form.node) payload.node_id = form.node;
        if (form.subscription) payload.subscription_id = form.subscription;
        if (editor?.item?.id) await patchJson(API.resources.proxyEgress(id), payload);
        else await postJson(API.resources.proxyEgresses, payload);
      } else if (kind === "nodes") {
        const id = editor?.item?.id || `proxy-node-${Date.now().toString(36)}`;
        const payload = { id, kind: "node", name: form.name.trim(), enabled: true, protocol: form.protocol, uri: form.uri.trim() };
        if (editor?.item?.id) await patchJson(API.resources.proxyNode(id), payload);
        else await postJson(API.resources.proxyNodes, payload);
      } else {
        const id = editor?.item?.id || `proxy-sub-${Date.now().toString(36)}`;
        const payload = { id, kind: "subscription", name: form.name.trim(), enabled: true, url: form.url.trim(), selection: form.selection, strategy: form.selection === "adaptive" ? "adaptive_topn_weighted" : "", top_n: form.selection === "adaptive" ? Number(form.topN || 2) : 1 };
        if (editor?.item?.id) await patchJson(API.resources.proxySubscription(id), payload);
        else await postJson(API.resources.proxySubscriptions, payload);
      }
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
      toast("代理配置已保存，正在应用", "success");
      setEditor(undefined);
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存代理配置失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (kind: Tab, item: any) => {
    if (item.id === "proxy-egress-default") { toast("默认代理出口不可删除", "error"); return; }
    const ok = await confirm({ title: `删除 ${item.name || item.id}？`, message: "引用该代理出口的策略可能失效。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      if (kind === "egress") await deleteJson(API.resources.proxyEgress(item.id));
      else if (kind === "nodes") await deleteJson(API.resources.proxyNode(item.id));
      else await deleteJson(API.resources.proxySubscription(item.id));
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
      toast("已删除，正在应用", "success");
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error", error?.requestId);
    }
  };

  const refreshSubscription = async (item: ProxySubscription) => {
    try {
      await postJson(API.resources.proxySubscriptionRefresh(item.id), {});
      toast("订阅刷新已提交", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
    } catch (error: any) {
      toast(error?.message || "刷新订阅失败", "error", error?.requestId);
    }
  };

  const columns = useMemo<ColumnDef<any, any>[]>(() => {
    if (tab === "egress") return [
      { id: "name", header: "出口名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><CloudCog size={14} /><strong>{text(row.original.name || row.original.id)}</strong>{row.original.id === "proxy-egress-default" && <Badge tone="neutral">默认</Badge>}</span>, meta: { width: 190 } },
      { id: "underlay", header: "承载 WAN", accessorFn: (row) => row.underlay_wan_id, cell: ({ getValue }) => <span>{underlays.find((item) => item.id === getValue())?.name || text(getValue(), "--")}</span>, meta: { width: 150 } },
      { id: "node", header: "节点/订阅", accessorFn: (row) => row.node_id || row.subscription_id, cell: ({ getValue }) => <span className="mono">{text(getValue(), "--")}</span>, meta: { width: 180 } },
      { id: "desc", header: "备注", accessorFn: (row) => row.description, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span> },
      { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open("egress", row.original)} disabled={session?.role === "readonly"}>编辑</Button>{row.original.id !== "proxy-egress-default" && <Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("egress", row.original)} disabled={session?.role === "readonly"}>删除</Button>}</div>, meta: { width: 130, align: "right", hideable: false } },
    ];
    if (tab === "nodes") return [
      { id: "name", header: "节点名称", accessorFn: (row) => row.name, cell: ({ getValue }) => text(getValue()), meta: { width: 180 } },
      { id: "protocol", header: "协议", accessorFn: (row) => row.protocol, cell: ({ getValue }) => <Badge tone="violet">{String(getValue() || "—").toUpperCase()}</Badge>, meta: { width: 85 } },
      { id: "uri", header: "URI", accessorFn: (row) => row.uri, cell: ({ getValue }) => <span className="mono truncate">{text(getValue())}</span>, meta: { width: 360 } },
      { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open("nodes", row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("nodes", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
    ];
    return [
      { id: "name", header: "订阅名称", accessorFn: (row) => row.name, cell: ({ getValue }) => text(getValue()), meta: { width: 180 } },
      { id: "url", header: "URL", accessorFn: (row) => row.url, cell: ({ getValue }) => <span className="mono truncate">{text(getValue())}</span>, meta: { width: 320 } },
      { id: "selection", header: "选择", accessorFn: (row) => row.selection, cell: ({ getValue }) => text(getValue() === "adaptive" ? "最小延迟" : "自动", "自动"), meta: { width: 100 } },
      { id: "top", header: "Top N", accessorFn: (row) => row.top_n, cell: ({ getValue }) => text(getValue(), "1"), meta: { width: 70, align: "right" } },
      { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<RotateCw size={13} />} onClick={() => refreshSubscription(row.original)}>刷新</Button><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open("subscriptions", row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove("subscriptions", row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 190, align: "right", hideable: false } },
    ];
  }, [tab, underlays, session?.role]);

  const data = tab === "egress" ? egressItems : tab === "nodes" ? nodeItems : subscriptionItems;
  const proxyLogs = logs.data?.logs || logs.data?.data?.logs || "";
  return (
    <div className="page">
      <PageHeader
        title="代理线路"
        subtitle="Xray 出口、节点、订阅与运行态；仅转发 VPP 已选择的流量"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { egresses.refetch(); nodes.refetch(); subscriptions.refetch(); status.refetch(); logs.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open(tab)} disabled={session?.role === "readonly"}>{tab === "egress" ? "新增出口" : tab === "nodes" ? "新增节点" : "新增订阅"}</Button></>}
      />
      <div className="status-strip">
        <span>Xray <Badge tone={status.data?.available ? "success" : "warning"} dot>{stateLabel(status.data?.state || (status.data?.available ? "available" : "unavailable"))}</Badge></span>
        <span>出口 <b>{egressItems.length}</b></span>
        <span>节点 <b>{nodeItems.length}</b></span>
        <span>订阅 <b>{subscriptionItems.length}</b></span>
        {status.data?.reason && <span className="muted">{status.data.reason}</span>}
      </div>
      <Tabs value={tab} onChange={(value) => setTab(value as Tab)} items={[{ key: "egress", label: "代理出口", count: egressItems.length }, { key: "nodes", label: "节点", count: nodeItems.length }, { key: "subscriptions", label: "订阅", count: subscriptionItems.length }]} />
      <DataTable data={data} columns={columns} loading={egresses.isLoading || nodes.isLoading || subscriptions.isLoading} error={null} onRetry={() => { egresses.refetch(); nodes.refetch(); subscriptions.refetch(); }} emptyTitle="暂无代理配置" emptyDescription="代理出口需要承载 WAN 与节点/订阅。" emptyAction={<Button size="sm" variant="primary" onClick={() => open(tab)} disabled={session?.role === "readonly"}>新增</Button>} getRowId={(row) => row.id} />
      <Card title="Xray 日志" subtitle="最近运行日志（只读）"><pre className="log-preview">{text(proxyLogs, "暂无日志输出。")}</pre></Card>

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor?.kind === "egress" ? "代理出口" : editor?.kind === "nodes" ? "代理节点" : "代理订阅"} size="md" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}>
        <div className="form-grid">
          <Field label="名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          {editor?.kind === "egress" && <>
            <Field label="承载 WAN" required><Select value={form.underlay || ""} onChange={(event) => setForm({ ...form, underlay: event.target.value })}><option value="">请选择 WAN</option>{underlays.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field>
            <Field label="节点"><Select value={form.node || ""} onChange={(event) => setForm({ ...form, node: event.target.value, subscription: "" })}><option value="">不指定</option>{nodeItems.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field>
            <Field label="订阅"><Select value={form.subscription || ""} onChange={(event) => setForm({ ...form, subscription: event.target.value, node: "" })}><option value="">不指定</option>{subscriptionItems.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field>
            <Field label="备注"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
          </>}
          {editor?.kind === "nodes" && <>
            <Field label="协议"><Select value={form.protocol || "vless"} onChange={(event) => setForm({ ...form, protocol: event.target.value })}><option value="vless">VLESS</option><option value="vmess">VMess</option><option value="trojan">Trojan</option><option value="ss">Shadowsocks</option></Select></Field>
            <Field label="URI" required className="span-2"><Textarea rows={4} value={form.uri || ""} onChange={(event) => setForm({ ...form, uri: event.target.value })} placeholder="vless://..." /></Field>
          </>}
          {editor?.kind === "subscriptions" && <>
            <Field label="订阅 URL" required className="span-2"><Textarea rows={3} value={form.url || ""} onChange={(event) => setForm({ ...form, url: event.target.value })} placeholder="https://..." /></Field>
            <Field label="节点选择"><Select value={form.selection || "fastest"} onChange={(event) => setForm({ ...form, selection: event.target.value })}><option value="fastest">自动选择</option><option value="adaptive">最小延迟</option></Select></Field>
            <Field label="Top N"><Select value={String(form.topN || 2)} onChange={(event) => setForm({ ...form, topN: Number(event.target.value) })}><option value="2">Top 2</option><option value="3">Top 3</option></Select></Field>
          </>}
        </div>
      </Modal>
    </div>
  );
}
