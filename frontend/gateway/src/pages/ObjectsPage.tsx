import { useMemo, useState, type FormEvent } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { Download, Eye, FileUp, Pencil, Plus, RefreshCw, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, downloadFile, patchJson, postJson, uploadFile } from "../api/client";
import { useObjectGroups } from "../api/hooks";
import type { ObjectGroup } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Drawer, Field, Input, Modal, PageHeader, Select, Tabs, Textarea, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { formatDateTime, splitLines, text } from "../lib/format";

function membersOf(group: ObjectGroup) {
  return group.members || group.entries || [];
}

function memberCount(group: ObjectGroup) {
  return membersOf(group).length || Number(group.source_entry_count || 0);
}

function ObjectGroupPage({ kind }: { kind: "domain" | "ip" }) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const groups = useObjectGroups();
  const [tab, setTab] = useState<"groups" | "members">("groups");
  const [editor, setEditor] = useState<ObjectGroup | null | undefined>(undefined);
  const [viewing, setViewing] = useState<ObjectGroup | null>(null);
  const [search, setSearch] = useState("");
  const [form, setForm] = useState<any>({});
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState(false);

  const kindItems = (groups.data?.items || []).filter((item) => item.kind === kind);
  const label = kind === "domain" ? "域名" : "IP";

  const open = (group?: ObjectGroup) => {
    setForm({
      name: group?.name || "",
      description: group?.description || "",
      members: group ? membersOf(group).join("\n") : "",
      format: group?.source?.format || "text",
      category: group?.source?.category || "CN",
      importMode: "append",
    });
    setFile(null);
    setEditor(group || null);
  };

  const save = async () => {
    if (!form.name?.trim()) { toast("请填写群组名称", "error"); return; }
    setBusy(true);
    try {
      const id = editor?.id || `${kind}-${Date.now().toString(36)}`;
      const payload: any = { id, name: form.name.trim(), kind, description: form.description || "", members: splitLines(form.members || ""), enabled: true };
      let saved: any;
      if (editor?.id) saved = await patchJson(API.resources.objectGroup(id), payload);
      else saved = await postJson(API.resources.objectGroups, payload);
      if (file) {
        const result = await uploadFile<any>(API.resources.objectImport(id), file, {
          kind,
          format: form.format || "text",
          category: form.category || "CN",
          mode: form.importMode || "append",
        });
        if (Array.isArray(result?.invalid_lines) && result.invalid_lines.length) {
          throw new Error(`导入存在 ${result.invalid_lines.length} 条无效内容`);
        }
      }
      await queryClient.invalidateQueries({ queryKey: ["resources", "object-groups"] });
      toast(`${label}组已保存`, "success");
      setEditor(undefined);
      setFile(null);
    } catch (error: any) {
      toast(error?.message || `保存${label}组失败`, "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (group: ObjectGroup) => {
    const ok = await confirm({ title: `删除 ${group.name || group.id}？`, message: "引用该组的策略可能失效。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      await deleteJson(API.resources.objectGroup(group.id));
      await queryClient.invalidateQueries({ queryKey: ["resources", "object-groups"] });
      toast("已删除", "success");
    } catch (error: any) {
      toast(error?.message || "删除失败", "error", error?.requestId);
    }
  };

  const exportGroup = async (group: ObjectGroup) => {
    try {
      const blob = await downloadFile(API.resources.objectExport(group.id));
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = `${group.name || group.id}.txt`;
      link.click();
      URL.revokeObjectURL(url);
    } catch (error: any) {
      toast(error?.message || "导出失败", "error", error?.requestId);
    }
  };

  const columns = useMemo<ColumnDef<ObjectGroup, any>[]>(() => [
    { id: "name", header: "群组名称", accessorFn: (row) => row.name, cell: ({ row }) => <span className="interface-name"><strong>{text(row.original.name || row.original.id)}</strong></span>, meta: { width: 200 } },
    { id: "description", header: "备注", accessorFn: (row) => row.description, cell: ({ getValue }) => <span className="muted">{text(getValue(), "")}</span>, meta: { width: 180 } },
    { id: "count", header: `${label}数量`, accessorFn: (row) => memberCount(row), cell: ({ getValue }) => Number(getValue()).toLocaleString("zh-CN"), meta: { width: 100, align: "right" } },
    { id: "format", header: "来源", accessorFn: (row) => row.source?.format, cell: ({ row }) => <Badge tone={row.original.source?.format ? "violet" : "neutral"}>{row.original.source?.format || "文本"}</Badge>, meta: { width: 90 } },
    { id: "updated", header: "更新时间", accessorFn: (row) => row.updated_at, cell: ({ getValue }) => <span className="muted">{formatDateTime(getValue())}</span>, meta: { width: 160 } },
    { id: "state", header: "状态", accessorFn: (row) => row.enabled === false ? "disabled" : "enabled", cell: ({ getValue }) => <Badge tone={getValue() === "enabled" ? "success" : "neutral"} dot>{getValue() === "enabled" ? "启用" : "停用"}</Badge>, meta: { width: 80 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Eye size={13} />} onClick={() => { setViewing(row.original); setTab("members"); }}>成员</Button><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Download size={13} />} onClick={() => exportGroup(row.original)}>导出</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly"}>删除</Button></div>, meta: { width: 230, align: "right", hideable: false } },
  ], [session?.role, label]);

  const viewingMembers = viewing ? membersOf(viewing).filter((member) => !search || String(member).toLowerCase().includes(search.toLowerCase())) : [];

  return (
    <div className="page">
      <PageHeader
        title={kind === "domain" ? "域名管理" : "IP 管理"}
        subtitle={kind === "domain" ? "域名组、成员与文件导入" : "IP 组、成员与文件导入"}
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => groups.refetch()}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly"}>新增{label}组</Button></>}
      />
      <Tabs value={tab} onChange={(value) => setTab(value as any)} items={[{ key: "groups", label: `${label}群组`, count: kindItems.length }, { key: "members", label: `${label}展示` }]} />
      {tab === "groups" ? (
        <DataTable data={kindItems} columns={columns} loading={groups.isLoading} error={groups.isError ? groups.error : null} onRetry={() => groups.refetch()} emptyTitle={`暂无${label}组`} emptyDescription={`创建${label}组后，可在策略中作为条件引用。`} emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly"}>新增{label}组</Button>} getRowId={(row) => row.id} />
      ) : (
        <div className="member-view">
          <div className="member-view-side">
            <div className="member-view-title">{label}组</div>
            {kindItems.map((group) => (
              <button type="button" key={group.id} className={viewing?.id === group.id ? "member-group is-active" : "member-group"} onClick={() => setViewing(group)}>
                <span>{group.name || group.id}</span><em>{memberCount(group)}</em>
              </button>
            ))}
          </div>
          <div className="member-view-main">
            <div className="member-view-head"><strong>{viewing?.name || `请选择${label}组`}</strong><label className="search-field"><Input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={`搜索${label}`} /></label></div>
            <div className="member-chip-list">
              {viewingMembers.map((member, index) => <code key={`${member}-${index}`}>{member}</code>)}
              {!viewingMembers.length && <p className="muted">{viewing ? "暂无成员" : `请选择左侧${label}组`}</p>}
            </div>
          </div>
        </div>
      )}

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor ? `编辑${label}组` : `新增${label}组`} size="md" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存</Button></>}>
        <div className="form-grid">
          <Field label="群组名" required><Input value={form.name || ""} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
          <Field label="备注"><Input value={form.description || ""} onChange={(event) => setForm({ ...form, description: event.target.value })} /></Field>
          <Field label={`${label}内容`} className="span-2" hint="一行一条；支持单个地址、网段或范围"><Textarea rows={8} value={form.members || ""} onChange={(event) => setForm({ ...form, members: event.target.value })} /></Field>
          <Field label="文件导入" className="span-2"><input type="file" onChange={(event) => setFile(event.target.files?.[0] || null)} /></Field>
          <Field label="格式"><Select value={form.format || "text"} onChange={(event) => setForm({ ...form, format: event.target.value })}><option value="text">文本</option><option value={kind === "domain" ? "geosite" : "geoip"}>{kind === "domain" ? "GeoSite .dat" : "GeoIP .dat"}</option></Select></Field>
          <Field label="分类"><Input value={form.category || "CN"} onChange={(event) => setForm({ ...form, category: event.target.value })} /></Field>
          <Field label="导入方式"><Select value={form.importMode || "append"} onChange={(event) => setForm({ ...form, importMode: event.target.value })}><option value="append">追加</option><option value="overwrite">覆盖</option></Select></Field>
        </div>
      </Modal>

      <Drawer open={Boolean(viewing)} onClose={() => setViewing(null)} title={viewing?.name || `${label}组`} subtitle={`${viewing ? memberCount(viewing) : 0} 条成员`} width="md">
        <div className="drawer-search"><Input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={`搜索${label}`} /></div>
        <div className="member-chip-list">{viewingMembers.map((member, index) => <code key={`${member}-${index}`}>{member}</code>)}</div>
        {!viewingMembers.length && <p className="muted">暂无成员</p>}
      </Drawer>
    </div>
  );
}

export function DomainsObjectPage() {
  return <ObjectGroupPage kind="domain" />;
}

export function IpObjectPage() {
  return <ObjectGroupPage kind="ip" />;
}
