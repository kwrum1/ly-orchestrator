import { useState, type FormEvent } from "react";
import { Eye, EyeOff, Loader2, LockKeyhole, ShieldCheck, Wifi } from "lucide-react";
import { useAuth } from "../state/providers";
import { Button, Field, Input } from "../components/ui";

function ProductMark({ size = 54 }: { size?: number }) {
  return (
    <span className="product-mark" style={{ width: size, height: size }} aria-hidden="true">
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round">
        <rect width="20" height="8" x="2" y="14" rx="2" />
        <path d="M6.01 18H6" />
        <path d="M10.01 18H10" />
        <path d="M15 10v4" />
        <path d="M17.84 7.17a4 4 0 0 0-5.66 0" />
        <path d="M20.66 4.34a8 8 0 0 0-11.31 0" />
      </svg>
    </span>
  );
}

export function LoginPage({ onLoggedIn, forceChange = false, initialUsername = "", product = "gateway" }: { onLoggedIn?: () => void; forceChange?: boolean; initialUsername?: string; product?: "gateway" | "orchestrator" }) {
  const config = product === "orchestrator"
    ? { brand: "流量编排控制台", title: "登录编排控制台", subtitle: "单机流量编排：本地拓扑、策略与服务链。", footer: "Ly Route Orchestrator", hint: "单机本地编排，不是多站点控制器" }
    : { brand: "出口网关控制台", title: "登录管理界面", subtitle: "使用设备管理员账号访问路由、策略与设备状态。", footer: "Ly Route Gateway", hint: "仅通过 HTTPS 访问管理界面" };
  const { login } = useAuth();
  const [username, setUsername] = useState(initialUsername);
  const [password, setPassword] = useState("");
  const [showPassword, setShowPassword] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [needsChange, setNeedsChange] = useState(forceChange);
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");

  const { changePassword } = useAuth();

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (!username.trim() || !password) {
      setError("请输入用户名和密码");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const result = await login(username.trim(), password);
      if (result.passwordChangeRequired) {
        setNeedsChange(true);
      } else {
        onLoggedIn?.();
      }
    } catch (err: any) {
      setError(err?.message || "登录失败");
    } finally {
      setBusy(false);
    }
  };

  const submitPassword = async (event: FormEvent) => {
    event.preventDefault();
    if (newPassword.length < 8) {
      setError("新密码至少 8 位");
      return;
    }
    if (newPassword !== confirmPassword) {
      setError("两次输入的新密码不一致");
      return;
    }
    if (newPassword.toLowerCase().includes(username.toLowerCase())) {
      setError("新密码不能包含用户名");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await changePassword(password, newPassword);
      onLoggedIn?.();
    } catch (err: any) {
      setError(err?.message || "修改密码失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <main className="login-page">
      <div className="login-grid" aria-hidden="true" />
      <section className="login-card">
        <div className="login-brand">
          <ProductMark />
          <div>
            <strong>Ly Route</strong>
            <span>{config.brand}</span>
          </div>
        </div>
        <div className="login-intro">
          <h1>{config.title}</h1>
          <p>{config.subtitle}</p>
        </div>
        {!needsChange ? (
          <form onSubmit={submit} className="login-form">
            <Field label="用户名">
              <Input
                autoFocus
                autoComplete="username"
                value={username}
                onChange={(event) => setUsername(event.target.value)}
                placeholder="admin"
              />
            </Field>
            <Field label="密码">
              <div className="password-input">
                <Input
                  type={showPassword ? "text" : "password"}
                  autoComplete="current-password"
                  value={password}
                  onChange={(event) => setPassword(event.target.value)}
                  placeholder="请输入密码"
                />
                <button type="button" onClick={() => setShowPassword((value) => !value)} aria-label={showPassword ? "隐藏密码" : "显示密码"}>
                  {showPassword ? <EyeOff size={16} /> : <Eye size={16} />}
                </button>
              </div>
            </Field>
            {error && <div className="login-error">{error}</div>}
            <Button type="submit" variant="primary" className="login-submit" loading={busy} disabled={busy}>
              登录
            </Button>
            <div className="login-hint">
              <ShieldCheck size={14} /> {config.hint}
            </div>
          </form>
        ) : (
          <form onSubmit={submitPassword} className="login-form">
            <div className="login-notice">
              <LockKeyhole size={18} />
              <div>
                <strong>首次登录需要修改密码</strong>
                <span>新密码至少 8 位，且不能包含用户名。</span>
              </div>
            </div>
            <Field label="当前密码">
              <Input type="password" value={password} onChange={(event) => setPassword(event.target.value)} autoComplete="current-password" />
            </Field>
            <Field label="新密码">
              <Input type="password" value={newPassword} onChange={(event) => setNewPassword(event.target.value)} autoComplete="new-password" />
            </Field>
            <Field label="确认新密码">
              <Input type="password" value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} autoComplete="new-password" />
            </Field>
            {error && <div className="login-error">{error}</div>}
            <Button type="submit" variant="primary" className="login-submit" loading={busy} disabled={busy}>
              保存并登录
            </Button>
          </form>
        )}
        <footer className="login-footer">
          <span><Wifi size={13} /> 管理口：当前访问地址</span>
          <span>{config.footer}</span>
        </footer>
      </section>
      <div className="login-side-note">
        <ProductMark size={30} />
        <span>{product === "orchestrator" ? "拓扑 · 策略 · 服务链" : "路由 · 策略 · 可观测"}</span>
      </div>
      {busy && <div className="login-busy"><Loader2 className="spin" size={16} /> 正在验证…</div>}
    </main>
  );
}
