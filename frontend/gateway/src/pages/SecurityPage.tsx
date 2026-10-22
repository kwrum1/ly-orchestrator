import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, RefreshCw, ShieldCheck, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useInterfacesResource, useSecurityAcls, useSecurityAttack, useSecurityIpMac, useSecurityThreat } from "../api/hooks";
import { AddressListEditor } from "../components/AddressListEditor";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, Tabs, Textarea, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { splitLines, splitTokens, text } from "../lib/format";

type Tab = "acl" | "ipmac" | "threat" | "attack";

function list(value: unknown): string[] {
  if (Array.isArray(value)) return value.map(String);
  if (typeof value === "string" && value.trim()) return value.split(/[\s,]+/).filter(Boolean);
  return [];
}

export function SecurityPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const interfaces = useInterfacesResource();
  const acls = useSecurityAcls();
  const ipmac = useSecurityIpMac();
  const threat = useSecurityThreat();
  const attack = useSecurityAttack();
  const [tab, setTab] = useState<Tab>("acl");
  const [editor, setEditor] = useState<any | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const candidates = (interfaces.data?.items || []).filter((item) => (item.gateway_role || item.role) !== "management");

  const current = {
    acl: acls.data?.items || [],
    ipmac: ipmac.data?.items || [],
    threat: threat.data?.items || [],
    attack: attack.data?.items || [],
  }[tab];

  const open = (item?: any) => {
    if (tab === "acl") {
      const match = item?.match || {};
      setForm({
        name: item?.name || "", priority: item?.priority || 100, action: item?.action || "deny", description: item?.description || "",
        sources: list(match.sources || match.src_ip), destinations: list(match.destinations || match.dst_ip), protocols: list(match.protocols || match.protocol || "any"),
        srcPort: match.src_port || "", dstPort: match.dst_port || "", direction: match.direction || "input",
      });
    } else if (tab === "ipmac") {
      setForm({
        name: item?.name || "", priority: item?.priority || 100, interfaceId: item?.interface_id || "", mode: item?.binding_mode || "alert",
        description: item?.description || "", bindings: (item?.bindings || []).map((entry: any) => `${entry.ip} ${entry.mac}`).join("\n"),
      });
    } else if (tab === "threat") {
      setForm({
        name: item?.name || "", priority: item?.priority || 100, interfaceId: item?.interface_id || "", direction: item?.direction || "both",
        listType: item?.list_type || "blacklist", entries: (item?.entries || []).join("\n"), description: item?.description || "",
      });
    } else {
      setForm({
        name: item?.name || "", priority: item?.priority || 100, interfaceId: item?.interface_id || "", attackType: item?.attack_type || "syn_flood",
        threshold: item?.threshold_pps || 1000, burst: item?.burst_packets || 2000, mode: item?.enforcement_mode || "alert",
        source: item?.source_prefix || "any", destination: item?.destination_prefix || "any", description: item?.description || "",
      });
    }
    setEditor(item || null);
  };

  const save = async () => {
    if (!form.name?.trim()) { toast("请填写名称", "error"); return; }
    if (!form.priority || Number(form.priority) < 1) { toast("优先级必须为正整数", "error"); return; }
    setBusy(true);
    try {
      const id = editor?.id || `sec-${tab}-${Date.now().toString(36)}`;
      let path: string = API.resources.securityAcls;
      let itemPath: (id: string) => string = API.resources.securityAcl;
      let payload: any = { id, kind: tab === "ipmac" ? "ip_mac_binding" : tab === "threat" ? "threat_intel" : tab === "attack" ? "attack_rule" : "acl", name: form.name.trim(), description: form.description || "", enabled: true, priority: Number(form.priority) };
      if (tab === "acl") {
        payload = { ...payload, action: form.action, match: { sources: form.sources || [], destinations: form.destinations || [], protocols: form.protocols?.length ? form.protocols : ["any"], src_port: form.srcPort || "", dst_port: form.dstPort || "", direction: form.direction } };
      } else if (tab === "ipmac") {
        path = API.resources.securityIpMac;
        itemPath = API.resources.securityIpMacItem;
        if (!form.interfaceId) throw new Error("请选择接口");
        const bindings = splitLines(form.bindings || "").map((line) => {
          const [ip, mac] = line.split(/\s+/);
          return { ip, mac };
        }).filter((entry) => entry.ip && entry.mac);
        if (!bindings.length) throw new Error("请至少填写一条 IP MAC 绑定");
        payload = { ...payload, interface_id: form.interfaceId, binding_mode: form.mode, unbound_behavior: form.mode === "enforce" ? "block" : "audit_only", bindings };
      } else if (tab === "threat") {
        path = API.resources.securityThreat;
        itemPath = API.resources.securityThreatItem;
        if (!form.interfaceId) throw new Error("请选择接口");
        payload = { ...payload, interface_id: form.interfaceId, direction: form.direction, list_type: form.listType, entries: splitLines(form.entries || "") };
        if (!payload.entries.length) throw new Error("请至少填写一条 IP/CIDR");
      } else {
        path = API.resources.securityAttack;
        itemPath = API.resources.securityAttackItem;
        if (!form.interfaceId) throw new Error("请选择接口");
        payload = { ...payload, interface_id: form.interfaceId, attack_type: form.attackType, threshold_pps: Number(form.threshold), burst_packets: Number(form.burst), enforcement_mode: form.mode, source_prefix: form.source || "any", destination_prefix: form.destination || "any" };
      }
      if (editor?.id) await patchJson(itemPath(id), payload);
      else await postJson(path, payload);
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
      toast("安全策略已保存，正在应用", "success");
      setEditor(undefined);
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存安全策略失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (item: any) => {
    const ok = await confirm({ title: `删除 ${item.name || item.id}？`, message: "删除后安全策略立即失效。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      const deletePath = tab === "acl" ? API.resources.securityAcl(item.id) : tab === "ipmac" ? API.resources.securityIpMacItem(item.id) : tab === "threat" ? API.resources.securityThreatItem(item.id) : API.resources.securityAttackItem(item.id);
      await deleteJson(deletePath);
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
      toast("已删除，正在应用", "success");
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "删除失败", "error", error?.requestId);
    }
  };

  const commonColumns = (kind: Tab): ColumnDef<any, any>[] => [
    { id: "name", header: "名称", accessorFn: (row: any) => row.name, cell: ({ row }: any) => <span className="interface-name"><ShieldCheck size={14} /><strong>{text(row.original.name || row.original.id)}</strong></span>, meta: { width: 200 } },
    { id: "priority", header: "优先级", accessorFn: (row: any) => row.priority, cell: ({ getValue }: any) => <span className="mono">{text(getValue())}</span>, meta: { width: 80, align: "right" } },
    kind === "acl" && { id: "action", header: "动作", accessorFn: (row: any) => row.action, cell: ({ getValue }: any) => <Badge tone={getValue() === "permit" ? "success" : "danger"}>{getValue() === "permit" ? "允许" : "拒绝"}</Badge>, meta: { width: 80 } },
    kind === "acl" && { id: "match", header: "匹配", accessorFn: (row: any) => row.match, cell: ({ row }: any) => { const match = row.original.match || {}; const source = Array.isArray(match.sources) ? match.sources.join(",") : match.src_ip || "任意"; const destination = Array.isArray(match.destinations) ? match.destinations.join(",") : match.dst_ip || "任意"; const protocol = Array.isArray(match.protocols) ? match.protocols.join(",") : match.protocol || "any"; return <span className="mono truncate" title={JSON.stringify(match)}>{match.direction || "—"} · {source} → {destination} · {String(protocol).toUpperCase()}</span>; }, meta: { width: 320 } },
    kind === "ipmac" && { id: "mode", header: "模式", accessorFn: (row: any) => row.binding_mode, cell: ({ getValue }: any) => <Badge tone="warning">{getValue() === "enforce" ? "强制" : "告警"}</Badge>, meta: { width: 80 } },
    kind === "ipmac" && { id: "count", header: "绑定数", accessorFn: (row: any) => row.bindings?.length || 0, cell: ({ getValue }: any) => text(getValue()), meta: { width: 80, align: "right" } },
    kind === "threat" && { id: "type", header: "类型", accessorFn: (row: any) => row.list_type, cell: ({ getValue }: any) => <Badge tone={getValue() === "blacklist" ? "danger" : "success"}>{getValue() === "blacklist" ? "黑名单" : "白名单"}</Badge>, meta: { width: 90 } },
    kind === "threat" && { id: "count", header: "条目", accessorFn: (row: any) => row.entries?.length || 0, cell: ({ getValue }: any) => text(getValue()), meta: { width: 80, align: "right" } },
    kind === "attack" && { id: "attackType", header: "攻击类型", accessorFn: (row: any) => row.attack_type, cell: ({ getValue }: any) => text(getValue()), meta: { width: 130 } },
    kind === "attack" && { id: "threshold", header: "阈值 pps", accessorFn: (row: any) => row.threshold_pps, cell: ({ getValue }: any) => text(getValue()), meta: { width: 100, align: "right" } },
    kind === "attack" && { id: "mode", header: "模式", accessorFn: (row: any) => row.enforcement_mode, cell: ({ getValue }: any) => <Badge tone={getValue() === "enforce" ? "danger" : "warning"}>{getValue() === "enforce" ? "阻断" : "告警"}</Badge>, meta: { width: 80 } },
    { id: "actions", header: "操作", cell: ({ row }: any) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly" || row.original.id === "sec-acl-default-deny-wan"} title={row.original.id === "sec-acl-default-deny-wan" ? "默认规则不可删除" : undefined}>删除</Button></div>, meta: { width: 120, align: "right", hideable: false } },
  ].filter(Boolean) as ColumnDef<any, any>[];

  return (
    <div className="page">
      <PageHeader
        title="安全策略"
        subtitle="L2–L4 基线安全：ACL、IP-MAC、威胁情报与攻击防护；不含 DPI/URL 过滤"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { acls.refetch(); ipmac.refetch(); threat.refetch(); attack.refetch(); }}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly"}>新增规则</Button></>}
      />
      <Tabs value={tab} onChange={(value) => setTab(value as Tab)} items={[
        { key: "acl", label: "ACL", count: acls.data?.items?.length },
        { key: "ipmac", label: "IP-MAC 绑定", count: ipmac.data?.items?.length },
        { key: "threat", label: "威胁情报", count: threat.data?.items?.length },
        { key: "attack", label: "攻击防护", count: attack.data?.items?.length },
      ]} />
      <DataTable data={current} columns={commonColumns(tab)} loading={acls.isLoading || ipmac.isLoading || threat.isLoading || attack.isLoading} error={acls.isError && tab === "acl" ? acls.error : null} onRetry={() => { acls.refetch(); ipmac.refetch(); threat.refetch(); attack.refetch(); }} emptyTitle="暂无安全规则" emptyDescription="安全规则需要选择接口并设置优先级。" emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly"}>新增规则</Button>} getRowId={(row) => row.id} />

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor ? "编辑安全规则" : "新增安全规则"} size="lg" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存并应用</Button></>}>
        <div className="form-grid">
          <Field label="名称" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="优先级" required><Input type="number" min={1} value={form.priority || 100} onChange={(event) => setForm({ ...form, priority: event.target.value })} /></Field>
          {tab !== "acl" ? <Field label="接口" required><Select value={form.interfaceId || ""} onChange={(event) => setForm({ ...form, interfaceId: event.target.value })}><option value="">请选择接口</option>{candidates.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field> : <Field label="方向"><Select value={form.direction || "input"} onChange={(event) => setForm({ ...form, direction: event.target.value })}><option value="input">入站</option><option value="output">出站</option><option value="both">双向</option></Select></Field>}
          {tab === "acl" && <>
            <Field label="动作" required><Select value={form.action || "deny"} onChange={(event) => setForm({ ...form, action: event.target.value })}><option value="deny">拒绝</option><option value="permit">允许</option></Select></Field>
            <Field label="协议"><Select value={form.protocols?.[0] || "any"} onChange={(event) => setForm({ ...form, protocols: [event.target.value] })}><option value="any">Any</option><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option></Select></Field>
            <Field label="源地址" className="span-2"><AddressListEditor value={form.sources || []} onChange={(value) => setForm({ ...form, sources: value })} /></Field>
            <Field label="目的地址" className="span-2"><AddressListEditor value={form.destinations || []} onChange={(value) => setForm({ ...form, destinations: value })} /></Field>
            <Field label="源端口"><Input value={form.srcPort || ""} onChange={(event) => setForm({ ...form, srcPort: event.target.value })} /></Field>
            <Field label="目的端口"><Input value={form.dstPort || ""} onChange={(event) => setForm({ ...form, dstPort: event.target.value })} /></Field>
          </>}
          {tab === "ipmac" && <>
            <Field label="模式"><Select value={form.mode || "alert"} onChange={(event) => setForm({ ...form, mode: event.target.value })}><option value="alert">告警（仅审计）</option><option value="enforce">强制（阻断未绑定）</option></Select></Field>
            <Field label="绑定列表" className="span-2" hint="每行：IP MAC"><Textarea rows={8} value={form.bindings || ""} onChange={(event) => setForm({ ...form, bindings: event.target.value })} placeholder="192.168.88.10 aa:bb:cc:dd:ee:ff" /></Field>
          </>}
          {tab === "threat" && <>
            <Field label="方向"><Select value={form.direction || "both"} onChange={(event) => setForm({ ...form, direction: event.target.value })}><option value="input">入站</option><option value="output">出站</option><option value="both">双向</option></Select></Field>
            <Field label="类型"><Select value={form.listType || "blacklist"} onChange={(event) => setForm({ ...form, listType: event.target.value })}><option value="blacklist">黑名单</option><option value="whitelist">白名单</option></Select></Field>
            <Field label="条目" className="span-2" hint="每行一个 IP 或 CIDR"><Textarea rows={8} value={form.entries || ""} onChange={(event) => setForm({ ...form, entries: event.target.value })} /></Field>
          </>}
          {tab === "attack" && <>
            <Field label="攻击类型"><Select value={form.attackType || "syn_flood"} onChange={(event) => setForm({ ...form, attackType: event.target.value })}><option value="syn_flood">SYN Flood</option><option value="udp_flood">UDP Flood</option><option value="icmp_flood">ICMP Flood</option><option value="new_connection_rate">新建连接速率</option></Select></Field>
            <Field label="执行模式"><Select value={form.mode || "alert"} onChange={(event) => setForm({ ...form, mode: event.target.value })}><option value="alert">告警</option><option value="enforce">阻断</option></Select></Field>
            <Field label="阈值 pps"><Input type="number" min={1} value={form.threshold || 1000} onChange={(event) => setForm({ ...form, threshold: event.target.value })} /></Field>
            <Field label="突发包数"><Input type="number" min={1} value={form.burst || 2000} onChange={(event) => setForm({ ...form, burst: event.target.value })} /></Field>
            <Field label="源前缀"><Input value={form.source || "any"} onChange={(event) => setForm({ ...form, source: event.target.value })} /></Field>
            <Field label="目的前缀"><Input value={form.destination || "any"} onChange={(event) => setForm({ ...form, destination: event.target.value })} /></Field>
          </>}
          <Field label="备注" className="span-2"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
        </div>
      </Modal>
    </div>
  );
}
