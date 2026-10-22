import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Globe2, Pencil, Plus, RefreshCw, Server, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useDnsPolicies, useDnsUpstreams, useObjectGroups, useProxyEgresses, useWanGroups, useWanLinks } from "../api/hooks";
import type { DnsPolicy, DnsUpstream } from "../api/types";
import { AddressListEditor } from "../components/AddressListEditor";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, Tabs, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { stateLabel, text } from "../lib/format";

const BOOTSTRAP = {
  domestic: ["223.5.5.5", "223.6.6.6", "119.29.29.29", "182.254.116.116", "180.76.76.76"],
  foreign: ["1.1.1.1", "1.0.0.1", "8.8.8.8", "8.8.4.4", "9.9.9.9"],
};

function lists(value: unknown): string[] {
  if (Array.isArray(value)) return value.map(String);
  if (typeof value === "string" && value.trim()) return value.split(/[\s,]+/).filter(Boolean);
  return [];
}

export function DnsPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const policies = useDnsPolicies();
  const upstreams = useDnsUpstreams();
  const groups = useObjectGroups();
  const links = useWanLinks();
  const wanGroups = useWanGroups();
  const proxies = useProxyEgresses();
  const [tab, setTab] = useState<"policies" | "upstreams">("policies");
  const [editor, setEditor] = useState<DnsPolicy | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const domainGroups = (groups.data?.items || []).filter((item) => item.kind === "domain");
  const egressOptions = [
    ...(links.data?.items || []).map((item) => ({ id: item.id, label: item.name || item.id })),
    ...(wanGroups.data?.items || []).map((item) => ({ id: item.id, label: `WAN组 · ${item.name || item.id}` })),
    ...(proxies.data?.items || []).filter((item) => item.id !== "proxy-egress-default").map((item) => ({ id: item.id, label: `代理 · ${item.name || item.id}` })),
  ];

  const open = (policy?: DnsPolicy) => {
    const rule = policy?.policy?.rules?.[0] || {};
    const outcome = rule.outcome || policy?.policy?.miss || {};
    const upstream = (upstreams.data?.items || []).find((item) => item.id === outcome.upstream_id);
    setForm({
      priority: policy?.priority || 1000,
      name: policy?.name || "",
      sources: lists(rule.source_prefixes),
      domainGroup: rule.domain_set_ids?.[0] || (policy?.policy?.rules?.length ? "" : "__any__"),
      action: outcome.kind === "reject" ? "reject" : "resolve",
      egress: upstream?.wan_egress_id || outcome.wan_egress_id || "",
      upstream: upstream?.servers?.[0] || outcome.fixed_answers?.[0] || "",
      bootstrap: upstream?.bootstrap_profile || "foreign",
      enabled: policy?.enabled !== false,
    });
    setEditor(policy || null);
  };

  const save = async () => {
    if (!form.priority || Number(form.priority) < 1 || Number(form.priority) > 65535) { toast("策略序号必须在 1-65535 之间", "error"); return; }
    if (form.action === "resolve" && (!form.egress || !form.upstream)) { toast("解析动作需要选择线路与上游", "error"); return; }
    setBusy(true);
    try {
      const id = editor?.id || `dns-${Date.now().toString(36)}`;
      const anyDomain = !form.domainGroup || form.domainGroup === "__any__";
      const upstreamID = `dns-policy-${id}-upstream`;
      const outcome = form.action === "reject"
        ? { kind: "reject" }
        : { kind: "direct", upstream_id: upstreamID, wan_egress_id: form.egress };
      if (form.action === "resolve") {
        const upstreamPayload = {
          id: upstreamID,
          name: `${form.name || id} 解析上游`,
          engine: "smartdns",
          enabled: true,
          servers: [form.upstream.trim()],
          bootstrap_profile: form.bootstrap === "none" ? "" : form.bootstrap,
          bootstrap_servers: form.bootstrap !== "none" && /^(https|h3):\/\//i.test(form.upstream) ? BOOTSTRAP[form.bootstrap as "domestic" | "foreign"] || BOOTSTRAP.foreign : [],
          wan_egress_id: form.egress,
          cache_size: 32768,
          ttl_min_seconds: 60,
          ttl_max_seconds: 600,
          prefetch: true,
        };
        const existing = (upstreams.data?.items || []).find((item) => item.id === upstreamID);
        if (existing) await patchJson(API.resources.dnsUpstream(upstreamID), upstreamPayload);
        else await postJson(API.resources.dnsUpstreams, upstreamPayload);
      }
      const policy = {
        name: form.name || id,
        engine: "smartdns",
        miss: anyDomain ? outcome : { kind: "reject" },
        rules: anyDomain ? [] : [{ id: `${id}-rule`, source_prefixes: form.sources || [], domains: [], domain_set_ids: [form.domainGroup], outcome }],
      };
      const payload = { id, kind: "policy", name: form.name || id, priority: Number(form.priority), enabled: form.enabled !== false, policy };
      if (editor?.id) await patchJson(API.resources.dnsPolicy(id), payload);
      else await postJson(API.resources.dnsPolicies, payload);
      toast("DNS 策略已保存，正在应用", "success");
      setEditor(undefined);
      await queryClient.invalidateQueries({ queryKey: ["resources", "dns-policies"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "dns-upstreams"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存 DNS 策略失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (policy: DnsPolicy) => {
    const ok = await confirm({ title: `删除 DNS 策略 ${policy.priority}？`, message: "删除后该域名会回落到默认解析行为。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      await deleteJson(API.resources.dnsPolicy(policy.id));
      toast("DNS 策略已删除，正在应用", "success");
      await queryClient.invalidateQueries({ queryKey: ["resources", "dns-policies"] });
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error", error?.requestId);
    }
  };

  const policyColumns = useMemo<ColumnDef<DnsPolicy, any>[]>(() => [
    { id: "priority", header: "序号", accessorFn: (row) => row.priority, cell: ({ getValue }) => <strong className="mono">{text(getValue())}</strong>, meta: { width: 74, align: "right" } },
    { id: "name", header: "名称", accessorFn: (row) => row.name, cell: ({ getValue }) => text(getValue()), meta: { width: 160 } },
    { id: "sources", header: "源 IP", accessorFn: (row) => lists(row.policy?.rules?.[0]?.source_prefixes), cell: ({ row }) => <span className="mono">{lists(row.original.policy?.rules?.[0]?.source_prefixes).join(", ") || "任意"}</span>, meta: { width: 160 } },
    { id: "domain", header: "目的域名", accessorFn: (row) => row.policy?.rules?.[0]?.domain_set_ids?.[0] || "所有域名", cell: ({ row }) => <Badge tone="info">{text(row.original.policy?.rules?.[0]?.domain_set_ids?.[0] || "所有域名")}</Badge>, meta: { width: 150 } },
    { id: "action", header: "动作", accessorFn: (row) => row.policy?.rules?.[0]?.outcome?.kind || row.policy?.miss?.kind, cell: ({ getValue }) => <Badge tone={getValue() === "reject" ? "danger" : "success"}>{getValue() === "reject" ? "拉黑" : "解析"}</Badge>, meta: { width: 80 } },
    { id: "upstream", header: "解析上游", accessorFn: (row) => row.policy?.rules?.[0]?.outcome?.upstream_id || row.policy?.miss?.upstream_id, cell: ({ getValue }) => <span className="mono">{text((upstreams.data?.items || []).find((item) => item.id === getValue())?.servers?.[0], "--")}</span>, meta: { width: 160 } },
    { id: "state", header: "状态", accessorFn: (row) => row.enabled === false ? "disabled" : row.runtime_state, cell: ({ row }) => <Badge tone={row.original.enabled === false ? "neutral" : "warning"} dot>{row.original.enabled === false ? "已停用" : stateLabel(row.original.runtime_state)}</Badge>, meta: { width: 90 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ], [session?.role, upstreams.data]);

  const upstreamColumns = useMemo<ColumnDef<DnsUpstream, any>[]>(() => [
    { id: "name", header: "名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><Server size={14} /><strong>{text(row.original.name || row.original.id)}</strong></span>, meta: { width: 180 } },
    { id: "servers", header: "服务器", accessorFn: (row) => row.servers, cell: ({ getValue }) => <span className="mono">{lists(getValue()).join(", ") || "--"}</span>, meta: { width: 260, mono: true } },
    { id: "bootstrap", header: "Bootstrap", accessorFn: (row) => row.bootstrap_profile, cell: ({ getValue }) => text(getValue(), "不使用"), meta: { width: 100 } },
    { id: "wan", header: "解析线路", accessorFn: (row) => row.wan_egress_id, cell: ({ getValue }) => <span>{egressOptions.find((item) => item.id === getValue())?.label || text(getValue(), "--")}</span>, meta: { width: 150 } },
    { id: "cache", header: "缓存", accessorFn: (row) => row.cache_size, cell: ({ getValue }) => text(getValue(), "--"), meta: { width: 80, align: "right" } },
  ], [egressOptions]);

  return (
    <div className="page">
      <PageHeader
        title="DNS 策略"
        subtitle="按序号匹配域名/源 IP，指定解析线路与上游；拉黑动作不解析"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { policies.refetch(); upstreams.refetch(); groups.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly"}>新增管控</Button></>}
      />
      <Tabs value={tab} onChange={(value) => setTab(value as any)} items={[{ key: "policies", label: "策略", count: policies.data?.items?.length }, { key: "upstreams", label: "解析上游", count: upstreams.data?.items?.length }]} />
      {tab === "policies" ? (
        <DataTable data={policies.data?.items || []} columns={policyColumns} loading={policies.isLoading} error={policies.isError ? policies.error : null} onRetry={() => policies.refetch()} emptyTitle="暂无 DNS 策略" emptyDescription="可创建所有域名的默认解析，或按域名组/源 IP 指定上游。" emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly"}>新增管控</Button>} getRowId={(row) => row.id} />
      ) : (
        <DataTable data={upstreams.data?.items || []} columns={upstreamColumns} loading={upstreams.isLoading} error={upstreams.isError ? upstreams.error : null} onRetry={() => upstreams.refetch()} emptyTitle="暂无解析上游" emptyDescription="创建 DNS 策略时会自动创建上游。" getRowId={(row) => row.id} />
      )}

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor ? "编辑 DNS 策略" : "新增 DNS 策略"} size="lg" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}>
        <div className="form-grid">
          <Field label="策略序号" required hint="1–65535，数字越小优先级越高"><Input type="number" min={1} max={65535} value={form.priority || ""} onChange={(event) => setForm({ ...form, priority: event.target.value })} /></Field>
          <Field label="策略名称"><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="源 IP / 网段" className="span-2"><AddressListEditor value={form.sources || []} onChange={(value) => setForm({ ...form, sources: value })} groups={(groups.data?.items || []).filter((item) => item.kind === "ip")} /></Field>
          <Field label="目的域名" required>
            <Select value={form.domainGroup || "__any__"} onChange={(event) => setForm({ ...form, domainGroup: event.target.value })}>
              <option value="__any__">所有域名</option>
              {domainGroups.map((group) => <option key={group.id} value={group.id}>{group.name || group.id}</option>)}
            </Select>
          </Field>
          <Field label="动作" required><Select value={form.action || "resolve"} onChange={(event) => setForm({ ...form, action: event.target.value })}><option value="resolve">解析</option><option value="reject">拉黑</option></Select></Field>
          {form.action !== "reject" && <>
            <Field label="解析线路" required><Select value={form.egress || ""} onChange={(event) => setForm({ ...form, egress: event.target.value })}><option value="">请选择线路</option>{egressOptions.map((item) => <option key={item.id} value={item.id}>{item.label}</option>)}</Select></Field>
            <Field label="Bootstrap DNS"><Select value={form.bootstrap || "foreign"} onChange={(event) => setForm({ ...form, bootstrap: event.target.value })}><option value="none">不使用</option><option value="domestic">国内内置</option><option value="foreign">境外内置</option></Select></Field>
            <Field label="解析上游" required className="span-2" hint="单个 IP 或 HTTPS DoH 地址"><Input value={form.upstream || ""} onChange={(event) => setForm({ ...form, upstream: event.target.value })} placeholder="1.1.1.1 或 https://dns.example/dns-query" /></Field>
          </>}
        </div>
      </Modal>
    </div>
  );
}
