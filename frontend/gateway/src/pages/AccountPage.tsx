import { useState } from "react";
import { KeyRound, LogOut } from "lucide-react";
import { Button, Card, Field, Input, PageHeader, useToast } from "../components/ui";
import { useAuth } from "../state/providers";

export function AccountPage() {
  const { session, changePassword, logout } = useAuth();
  const { toast } = useToast();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    if (!current || !next) { toast("请输入当前密码和新密码", "error"); return; }
    if (next.length < 8) { toast("新密码至少 8 位", "error"); return; }
    if (next !== confirm) { toast("两次输入的新密码不一致", "error"); return; }
    setBusy(true);
    try {
      await changePassword(current, next);
      toast("密码已更新", "success");
      setCurrent(""); setNext(""); setConfirm("");
    } catch (error: any) {
      toast(error?.message || "修改密码失败", "error");
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page">
      <PageHeader title="管理员账号" subtitle="单管理员账号与密码" />
      <div className="config-grid">
        <Card title="账号" subtitle="修改管理员密码">
          <div className="form-stack">
            <Field label="用户名"><Input value={session?.username || "admin"} disabled /></Field>
            <Field label="当前密码" required><Input type="password" autoComplete="current-password" value={current} onChange={(event) => setCurrent(event.target.value)} /></Field>
            <Field label="新密码" required hint="至少 8 位"><Input type="password" autoComplete="new-password" value={next} onChange={(event) => setNext(event.target.value)} /></Field>
            <Field label="确认新密码" required><Input type="password" autoComplete="new-password" value={confirm} onChange={(event) => setConfirm(event.target.value)} /></Field>
            <Button variant="primary" icon={<KeyRound size={14} />} loading={busy} onClick={submit}>确定</Button>
          </div>
        </Card>
        <Card title="当前会话" subtitle="登录状态">
          <dl className="fact-list">
            <div><dt>用户名</dt><dd>{session?.username || "admin"}</dd></div>
            <div><dt>角色</dt><dd>{session?.role === "readonly" ? "只读" : "管理员"}</dd></div>
            <div><dt>状态</dt><dd>在线</dd></div>
          </dl>
          <Button icon={<LogOut size={14} />} onClick={() => logout()}>退出登录</Button>
        </Card>
      </div>
    </div>
  );
}
