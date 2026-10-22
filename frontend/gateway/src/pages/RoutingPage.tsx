import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowRight, Pencil, Plus, RefreshCw, Route, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useObjectGroups, useProxyEgresses, useRoutes, useWanGroups, useWanLinks } from "../api/hooks";
import type { RoutePolicy } from "../api/types";
import { AddressListEditor } from "../components/AddressListEditor";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, Select, Switch, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { stateLabel, text } from "../lib/format";

function matchList(value: unknown): string[] {
  if (Array.isArray(value)) return value.map(String);
  if (typeof value === "string" && value.trim()) return value.split(/[\s,]+/).filter(Boolean);
  return [];
}

function summary(list: string[]) {
  return list.length ? list.join(", ") : "任意";
}

export function RoutingPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const routes = useRoutes();
  const links = useWanLinks();
  const groups = useWanGroups();
  const proxies = useProxyEgresses();
  const objects = useObjectGroups();
  const [editor, setEditor] = useState<RoutePolicy | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const wanItems = links.data?.items || [];
  const proxyItems = (proxies.data?.items || []).filter((item) => item.id !== "proxy-egress-default");
  const domainGroups = (objects.data?.items || []).filter((item) => item.kind === "domain");
  const egressOptions = [
    ...wanItems.map((item) => ({ id: item.id, label: item.name || item.id, group: "WAN" })),
    ...(groups.data?.items || []).map((item) => ({ id: item.id, label: item.name || item.id, group: "WAN 群组" })),
    ...proxyItems.map((item) => ({ id: item.id, label: item.name || item.id, group: "代理" })),
  ];

  const open = (route?: RoutePolicy) => {
    const match = route?.match || {};
    setForm({
      priority: route?.priority || (routes.data?.items?.length ? 100 : 65535),
      name: route?.name || (routes.data?.items?.length ? "" : "默认上网"),
      action: route?.action === "route" ? "route" : "nat",
      sources: matchList(match.sources || match.src_ip),
      destinations: matchList(match.destinations || match.dst_ip),
      sourcePort: match.src_port || "",
      destPort: match.dst_port || "",
      protocols: matchList(match.protocols || match.protocol || "any"),
      domainGroup: match.domain_group || "",
      egress: route?.egress || egressOptions[0]?.id || "",
      nextHop: route?.next_hop || "",
      fullCone: Boolean(route?.full_cone),
      enabled: route?.enabled !== false,
    });
    setEditor(route || null);
  };

  const save = async () => {
    if (!form.priority || Number(form.priority) < 1 || Number(form.priority) > 65535) {
      toast("策略序号必须在 1-65535 之间", "error");
      return;
    }
    if (form.action === "nat" && !form.egress) {
      toast("NAT 动作需要选择出口", "error");
      return;
    }
    setBusy(true);
    try {
      const id = editor?.id || `route-${form.priority}-${Date.now().toString(36)}`;
      const match: any = {
        sources: form.sources || [],
        destinations: form.destinations || [],
        src_port: form.sourcePort || "",
        dst_port: form.destPort || "",
        protocols: form.protocols?.length ? form.protocols : ["any"],
      };
      if (form.domainGroup) match.domain_group = form.domainGroup;
      const payload = {
        id,
        name: form.name || "",
        priority: Number(form.priority),
        enabled: form.enabled !== false,
        action: form.action,
        match,
        egress: form.egress || "",
        next_hop: form.nextHop || "",
        full_cone: Boolean(form.fullCone && form.action === "nat"),
      };
      if (editor?.id) await patchJson(API.resources.route(id), payload);
      else await postJson(API.resources.routes, payload);
      toast("策略已保存，正在应用", "success");
      setEditor(undefined);
      await queryClient.invalidateQueries({ queryKey: ["resources", "routes"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存策略失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (route: RoutePolicy) => {
    const ok = await confirm({ title: `删除策略 ${route.priority}？`, message: "删除后流量会继续匹配下一条策略，可能改变出口。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      await deleteJson(API.resources.route(route.id));
      toast("策略已删除，正在应用", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources", "routes"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除策略失败", "error", error?.requestId);
    }
  };

  const columns = useMemo<ColumnDef<RoutePolicy, any>[]>(() => [
    { id: "priority", header: "序号", accessorFn: (row) => row.priority, cell: ({ getValue }) => <strong className="mono">{text(getValue())}</strong>, meta: { width: 78, align: "right" } },
    { id: "action", header: "类型", accessorFn: (row) => row.action, cell: ({ getValue }) => <Badge tone={getValue() === "route" ? "violet" : "info"}>{(getValue() === "route" ? "路由" : "NAT").toUpperCase()}</Badge>, meta: { width: 80 } },
    { id: "sources", header: "源地址", accessorFn: (row) => matchList(row.match?.sources || row.match?.src_ip), cell: ({ row }) => <span className="mono truncate" title={summary(matchList(row.original.match?.sources || row.original.match?.src_ip))}>{summary(matchList(row.original.match?.sources || row.original.match?.src_ip))}</span>, meta: { width: 170, mono: true } },
    { id: "destinations", header: "目的地址", accessorFn: (row) => matchList(row.match?.destinations || row.match?.dst_ip), cell: ({ row }) => <span className="mono truncate" title={summary(matchList(row.original.match?.destinations || row.original.match?.dst_ip))}>{summary(matchList(row.original.match?.destinations || row.original.match?.dst_ip))}</span>, meta: { width: 170, mono: true } },
    { id: "ports", header: "端口", accessorFn: (row) => `${row.match?.src_port || "任意"} / ${row.match?.dst_port || "任意"}`, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 130 } },
    { id: "egress", header: "目标线路", accessorFn: (row) => row.egress, cell: ({ getValue }) => <span>{egressOptions.find((item) => item.id === getValue())?.label || text(getValue(), "--")}</span>, meta: { width: 150 } },
    { id: "state", header: "状态", accessorFn: (row) => row.enabled === false ? "disabled" : row.runtime_state, cell: ({ row }) => <Badge tone={row.original.enabled === false ? "neutral" : row.original.runtime_state === "applied" ? "success" : "warning"} dot>{row.original.enabled === false ? "已停用" : stateLabel(row.original.runtime_state)}</Badge>, meta: { width: 95 } },
    { id: "name", header: "备注", accessorFn: (row) => row.name, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span>, meta: { width: 130 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [egressOptions, session?.role]);

  return (
    <div className="page">
      <PageHeader
        title="策略路由"
        subtitle="序号小优先；空匹配条件表示任意；NAT 出口来自 WAN、WAN 群组或代理"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { routes.refetch(); links.refetch(); groups.refetch(); proxies.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly"}>新增策略</Button></>}
      />

      <div className="policy-hint">
        <Route size={16} />
        <div><strong>默认上网策略</strong><span>通常创建一条序号 65535、条件任意、动作 NAT 的策略；后续策略使用更小序号优先匹配。</span></div>
        {!(routes.data?.items || []).some((route) => route.action === "nat" && !matchList(route.match?.sources).length && !matchList(route.match?.destinations).length) && (
          <Button size="sm" variant="secondary" onClick={() => { open(); setForm((current: any) => ({ ...current, priority: 65535, name: "默认上网", action: "nat", sources: [], destinations: [], sourcePort: "", destPort: "", protocols: ["any"], egress: egressOptions[0]?.id || "" })); }} disabled={session?.role === "readonly"}>
            创建默认策略
          </Button>
        )}
      </div>

      <DataTable
        data={routes.data?.items || []}
        columns={columns}
        loading={routes.isLoading}
        error={routes.isError ? routes.error : null}
        onRetry={() => routes.refetch()}
        emptyTitle="暂无策略路由"
        emptyDescription="没有策略时，流量不会按预期选择出口；建议先创建默认 Any→Any NAT。"
        emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly"}>新增策略</Button>}
        getRowId={(row) => row.id}
      />

      <Modal
        open={editor !== undefined}
        onClose={() => !busy && setEditor(undefined)}
        title={editor ? "编辑策略" : "新增策略"}
        subtitle="策略按序号从小到大匹配；保存后需要应用运行态"
        size="xl"
        busy={busy}
        footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}
      >
        <div className="policy-form">
          <section>
            <h3>基本信息</h3>
            <div className="form-grid">
              <Field label="策略序号" required hint="1–65535，数字越小优先级越高"><Input type="number" min={1} max={65535} value={form.priority || ""} onChange={(event) => setForm({ ...form, priority: event.target.value })} /></Field>
              <Field label="策略备注"><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} placeholder="例如：默认上网" /></Field>
              <Field label="启用"><Switch checked={form.enabled !== false} onChange={(value) => setForm({ ...form, enabled: value })} label="启用此策略" /></Field>
            </div>
          </section>
          <section>
            <h3>匹配条件</h3>
            <div className="form-grid">
              <Field label="源地址" className="span-2"><AddressListEditor value={form.sources || []} onChange={(value) => setForm({ ...form, sources: value })} groups={(objects.data?.items || []).filter((item) => item.kind === "ip")} /></Field>
              <Field label="目的地址" className="span-2"><AddressListEditor value={form.destinations || []} onChange={(value) => setForm({ ...form, destinations: value })} groups={(objects.data?.items || []).filter((item) => item.kind === "ip")} /></Field>
              <Field label="目的域名组"><Select value={form.domainGroup || ""} onChange={(event) => setForm({ ...form, domainGroup: event.target.value })}><option value="">不选择</option>{domainGroups.map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}</Select></Field>
              <Field label="协议"><Select value={form.protocols?.[0] || "any"} onChange={(event) => setForm({ ...form, protocols: [event.target.value] })}><option value="any">Any</option><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option></Select></Field>
              <Field label="源端口"><Input value={form.sourcePort || ""} onChange={(event) => setForm({ ...form, sourcePort: event.target.value })} placeholder="任意" /></Field>
              <Field label="目的端口"><Input value={form.destPort || ""} onChange={(event) => setForm({ ...form, destPort: event.target.value })} placeholder="任意" /></Field>
            </div>
          </section>
          <section>
            <h3>执行动作</h3>
            <div className="form-grid">
              <Field label="动作" required><Select value={form.action || "nat"} onChange={(event) => setForm({ ...form, action: event.target.value })}><option value="nat">NAT</option><option value="route">路由</option></Select></Field>
              <Field label={form.action === "route" ? "路由线路" : "NAT 线路"} required>
                <Select value={form.egress || ""} onChange={(event) => setForm({ ...form, egress: event.target.value })}>
                  <option value="">请选择</option>
                  {egressOptions.map((item) => <option key={item.id} value={item.id}>{item.group} · {item.label}</option>)}
                </Select>
              </Field>
              <Field label="下一跳"><Input value={form.nextHop || ""} onChange={(event) => setForm({ ...form, nextHop: event.target.value })} placeholder="可为空" /></Field>
              {form.action === "nat" && <Field label="全锥型 NAT"><Switch checked={Boolean(form.fullCone)} onChange={(value) => setForm({ ...form, fullCone: value })} label="启用全锥型 NAT" /></Field>}
            </div>
          </section>
        </div>
      </Modal>
    </div>
  );
}
