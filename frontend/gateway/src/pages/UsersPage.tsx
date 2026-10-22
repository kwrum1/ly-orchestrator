import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { KeyRound, Pencil, Plus, RefreshCw, Trash2, UserRound } from "lucide-react";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { useUsers } from "../api/hooks";
import type { AuthUser } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Field, Input, Modal, PageHeader, Select, useToast } from "../components/ui";
import { useAuth, useConfirm } from "../state/providers";
import { text } from "../lib/format";

export function UsersPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const users = useUsers();
  const [editor, setEditor] = useState<AuthUser | null | undefined>(undefined);
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);

  const open = (user?: AuthUser) => {
    setForm({ username: user?.username || "", role: user?.role || "readonly", password: "", confirm: "" });
    setEditor(user || null);
  };

  const save = async () => {
    if (!form.username?.trim()) { toast("请填写用户名", "error"); return; }
    if (!editor && form.password.length < 8) { toast("密码至少 8 位", "error"); return; }
    if (form.password && form.password !== form.confirm) { toast("两次输入的密码不一致", "error"); return; }
    if (form.password && form.password.toLowerCase().includes(form.username.toLowerCase())) { toast("密码不能包含用户名", "error"); return; }
    setBusy(true);
    try {
      if (editor) {
        const payload: any = { username: editor.username, role: form.role };
        if (form.password) payload.password = form.password;
        await patchJson(API.auth.user(editor.username), payload);
      } else {
        await postJson(API.auth.users, { username: form.username.trim(), role: form.role, password: form.password });
      }
      await queryClient.invalidateQueries({ queryKey: ["resources", "auth-users"] });
      toast(editor ? "用户已更新" : "用户已创建", "success");
      setEditor(undefined);
    } catch (error: any) {
      toast(error?.message || "保存用户失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const remove = async (user: AuthUser) => {
    const ok = await confirm({ title: `删除用户 ${user.username}？`, message: "删除后该账号无法再登录。", confirmLabel: "删除", danger: true });
    if (!ok) return;
    try {
      await deleteJson(API.auth.user(user.username));
      await queryClient.invalidateQueries({ queryKey: ["resources", "auth-users"] });
      toast("用户已删除", "success");
    } catch (error: any) {
      toast(error?.message || "删除用户失败", "error", error?.requestId);
    }
  };

  const columns = useMemo<ColumnDef<AuthUser, any>[]>(() => [
    { id: "username", header: "用户名", accessorFn: (row) => row.username, cell: ({ row }) => <span className="interface-name"><UserRound size={14} /><strong>{text(row.original.username)}</strong></span>, meta: { width: 200 } },
    { id: "role", header: "角色", accessorFn: (row) => row.role, cell: ({ getValue }) => <Badge tone={getValue() === "admin" ? "info" : "neutral"}>{getValue() === "admin" ? "管理员" : "只读"}</Badge>, meta: { width: 100 } },
    { id: "state", header: "状态", accessorFn: (row) => row.enabled !== false, cell: ({ getValue }) => <Badge tone={getValue() ? "success" : "neutral"} dot>{getValue() ? "启用" : "停用"}</Badge>, meta: { width: 90 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<Pencil size={13} />} onClick={() => open(row.original)} disabled={session?.role === "readonly"}>编辑</Button><Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => remove(row.original)} disabled={session?.role === "readonly" || row.original.username === "admin" || row.original.username === session?.username}>删除</Button></div>, meta: { width: 140, align: "right", hideable: false } },
  ], [session?.role, session?.username]);

  return (
    <div className="page">
      <PageHeader
        title="用户管理"
        subtitle="管理员与只读账号；admin 与当前用户不可删除"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => users.refetch()}>刷新</Button><Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => open()} disabled={session?.role === "readonly"}>新增账号</Button></>}
      />
      <DataTable data={users.data?.items || []} columns={columns} loading={users.isLoading} error={users.isError ? users.error : null} onRetry={() => users.refetch()} emptyTitle="暂无系统用户" emptyDescription="创建管理员或只读账号。" emptyAction={<Button size="sm" variant="primary" onClick={() => open()} disabled={session?.role === "readonly"}>新增账号</Button>} getRowId={(row) => row.username} />

      <Modal open={editor !== undefined} onClose={() => !busy && setEditor(undefined)} title={editor ? "编辑用户" : "新增账号"} size="sm" busy={busy} footer={<><Button onClick={() => setEditor(undefined)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={save}>保存</Button></>}>
        <div className="form-stack">
          <Field label="用户名" required><Input value={form.username || ""} onChange={(event) => setForm({ ...form, username: event.target.value })} disabled={Boolean(editor)} /></Field>
          <Field label="角色"><Select value={form.role || "readonly"} onChange={(event) => setForm({ ...form, role: event.target.value })}><option value="readonly">只读</option><option value="admin">管理员</option></Select></Field>
          <Field label={editor ? "新密码（留空不修改）" : "密码"} required={!editor} hint="至少 8 位，不能包含用户名"><Input type="password" autoComplete="new-password" value={form.password || ""} onChange={(event) => setForm({ ...form, password: event.target.value })} /></Field>
          <Field label="确认密码" required={!editor}><Input type="password" autoComplete="new-password" value={form.confirm || ""} onChange={(event) => setForm({ ...form, confirm: event.target.value })} /></Field>
          <div className="inline-warning"><KeyRound size={14} /> 只读账号不能修改配置，但仍可查看页面与运行态。</div>
        </div>
      </Modal>
    </div>
  );
}
