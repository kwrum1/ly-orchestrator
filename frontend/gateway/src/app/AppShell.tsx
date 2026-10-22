import { useEffect, useMemo, useRef, useState } from "react";
import { NavLink, Outlet, useLocation, useNavigate } from "react-router-dom";
import {
  Activity,
  ChevronDown,
  ChevronRight,
  CircleHelp,
  Command,
  LogOut,
  Menu,
  PanelLeftClose,
  PanelLeftOpen,
  RefreshCw,
  Search,
  ShieldCheck,
  UserRound,
} from "lucide-react";
import { ALL_PAGES, COMMAND_ACTIONS, NAV_GROUPS } from "./nav";
import { useAuth, useApply, useConfirm } from "../state/providers";
import { useHealth, useRuntime } from "../api/hooks";
import { Badge, Button, Drawer, Field, IconButton, Input, Modal, useToast } from "../components/ui";
import { ApplyBanner } from "../state/providers";
import { classNames, formatDateTime, stateLabel } from "../lib/format";

export function AppShell() {
  const location = useLocation();
  const navigate = useNavigate();
  const { session, logout, changePassword } = useAuth();
  const { toast } = useToast();
  const confirm = useConfirm();
  const health = useHealth();
  const runtime = useRuntime();
  const apply = useApply();

  const [collapsed, setCollapsed] = useState(() => localStorage.getItem("lyroute.sidebar.collapsed") === "1");
  const [mobileOpen, setMobileOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [healthOpen, setHealthOpen] = useState(false);
  const [commandOpen, setCommandOpen] = useState(false);
  const [commandQuery, setCommandQuery] = useState("");
  const [userOpen, setUserOpen] = useState(false);
  const [passwordOpen, setPasswordOpen] = useState(false);
  const [passwordBusy, setPasswordBusy] = useState(false);
  const [passwordError, setPasswordError] = useState("");
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");

  const currentPage = ALL_PAGES.find((page) => page.path === location.pathname);
  const currentGroup = NAV_GROUPS.find((group) => group.pages.some((page) => page.path === location.pathname));

  const filteredGroups = useMemo(() => {
    const value = query.trim().toLowerCase();
    if (!value) return NAV_GROUPS;
    return NAV_GROUPS
      .map((group) => ({
        ...group,
        pages: group.pages.filter((page) => `${group.label} ${page.label} ${page.description}`.toLowerCase().includes(value)),
      }))
      .filter((group) => group.pages.length > 0);
  }, [query]);

  const commands = useMemo(() => {
    const value = commandQuery.trim().toLowerCase();
    const pages = ALL_PAGES.filter((page) => !value || `${page.label} ${page.description} ${page.path}`.toLowerCase().includes(value));
    return { pages, actions: COMMAND_ACTIONS.filter((action) => !value || action.label.toLowerCase().includes(value)) };
  }, [commandQuery]);

  useEffect(() => {
    const onKeyDown = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "k") {
        event.preventDefault();
        setCommandOpen((value) => !value);
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, []);

  useEffect(() => {
    setMobileOpen(false);
  }, [location.pathname]);

  const toggleCollapsed = () => {
    setCollapsed((value) => {
      localStorage.setItem("lyroute.sidebar.collapsed", value ? "0" : "1");
      return !value;
    });
  };

  const degraded = (health.data?.dependencies || []).filter((item) => item.available === false);
  const healthTone = health.isError ? "danger" : degraded.length ? "warning" : health.data?.status === "ok" ? "success" : "warning";
  const healthText = health.isLoading ? "读取中" : health.isError ? "状态未知" : degraded.length ? `降级 ${degraded.length}` : "运行正常";
  const runtimeComponents = runtime.data?.components || [];
  const runtimeDegraded = runtimeComponents.filter((component) => component.available === false || component.state === "degraded");

  const submitPassword = async () => {
    if (newPassword.length < 8) {
      setPasswordError("新密码至少 8 位");
      return;
    }
    if (newPassword !== confirmPassword) {
      setPasswordError("两次输入的新密码不一致");
      return;
    }
    setPasswordBusy(true);
    setPasswordError("");
    try {
      await changePassword(currentPassword, newPassword);
      toast("密码已修改", "success");
      setPasswordOpen(false);
      setCurrentPassword("");
      setNewPassword("");
      setConfirmPassword("");
    } catch (error: any) {
      setPasswordError(error?.message || "修改密码失败");
    } finally {
      setPasswordBusy(false);
    }
  };

  const handleLogout = async () => {
    if (!(await confirm({ title: "退出登录？", message: "当前会话将立即失效。", confirmLabel: "退出" }))) return;
    await logout();
    navigate("/", { replace: true });
  };

  const runCommand = (key: string, path?: string) => {
    setCommandOpen(false);
    if (path) {
      navigate(path);
      return;
    }
    if (key === "apply") {
      void apply.apply();
      return;
    }
    if (key === "refresh") {
      window.dispatchEvent(new CustomEvent("lyroute:refresh"));
      toast("正在刷新当前页面", "info");
    }
  };

  return (
    <div className={classNames("app-shell", collapsed && "is-collapsed", mobileOpen && "mobile-open")}>
      <header className="topbar">
        <div className="topbar-left">
          <IconButton label="打开菜单" className="mobile-only" onClick={() => setMobileOpen(true)}><Menu size={18} /></IconButton>
          <button className="brand" type="button" onClick={() => navigate("/")}>
            <span className="brand-mark">LR</span>
            <span className="brand-copy">
              <strong>Ly Route</strong>
              <small>出口网关</small>
            </span>
          </button>
          <div className="breadcrumb">
            <span>{currentGroup?.label || "概览"}</span>
            <ChevronRight size={13} />
            <strong>{currentPage?.label || "总览"}</strong>
          </div>
        </div>
        <div className="topbar-right">
          <button className="global-search" type="button" onClick={() => setCommandOpen(true)}>
            <Search size={15} />
            <span>搜索页面或动作</span>
            <kbd><Command size={11} /> K</kbd>
          </button>
          <button className={`health-pill health-${healthTone}`} type="button" onClick={() => setHealthOpen(true)}>
            <i className="status-dot" />
            <span>{healthText}</span>
          </button>
          {apply.running && (
            <Badge tone="info" dot>应用中</Badge>
          )}
          <div className="user-menu">
            <button className="user-button" type="button" onClick={() => setUserOpen((value) => !value)}>
              <span className="avatar">{session?.username?.slice(0, 1).toUpperCase() || "A"}</span>
              <span className="user-meta">
                <strong>{session?.username || "admin"}</strong>
                <small>{session?.role === "readonly" ? "只读" : "管理员"}</small>
              </span>
              <ChevronDown size={14} />
            </button>
            {userOpen && (
              <div className="user-popover" onMouseLeave={() => setUserOpen(false)}>
                <button type="button" onClick={() => { setUserOpen(false); setPasswordOpen(true); }}>
                  <ShieldCheck size={15} /> 修改密码
                </button>
                <button type="button" onClick={handleLogout}><LogOut size={15} /> 退出登录</button>
              </div>
            )}
          </div>
        </div>
      </header>

      <aside className="sidebar">
        <div className="sidebar-top">
          {!collapsed && (
            <label className="side-search">
              <Search size={14} />
              <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="筛选菜单" />
            </label>
          )}
          <IconButton label={collapsed ? "展开侧栏" : "收起侧栏"} onClick={toggleCollapsed}>
            {collapsed ? <PanelLeftOpen size={17} /> : <PanelLeftClose size={17} />}
          </IconButton>
        </div>
        <nav className="side-nav" aria-label="主导航">
          {filteredGroups.map((group) => (
            <section className="nav-group" key={group.key}>
              {!collapsed && <h3>{group.label}</h3>}
              {group.pages.map((page) => (
                <NavLink
                  key={page.key}
                  to={page.path}
                  end={page.path === "/"}
                  className={({ isActive }) => classNames("nav-item", isActive && "is-active")}
                  title={collapsed ? page.label : page.description}
                >
                  {page.icon}
                  {!collapsed && <span>{page.label}</span>}
                </NavLink>
              ))}
            </section>
          ))}
        </nav>
        <div className="sidebar-foot">
          <span className={classNames("status-dot", runtimeDegraded.length ? "status-dot-warning" : "status-dot-success")} />
          {!collapsed && <span>{runtimeDegraded.length ? `${runtimeDegraded.length} 个组件降级` : "组件正常"}</span>}
        </div>
      </aside>

      {mobileOpen && <div className="mobile-overlay" onClick={() => setMobileOpen(false)} />}

      <main className="main">
        <ApplyBanner />
        <div className="main-inner">
          <Outlet />
        </div>
      </main>

      <Drawer
        open={healthOpen}
        onClose={() => setHealthOpen(false)}
        title="设备健康"
        subtitle={`采样于 ${formatDateTime(health.data?.request_id ? new Date() : new Date())}`}
        width="sm"
      >
        <div className="health-summary">
          <Badge tone={healthTone === "success" ? "success" : healthTone === "danger" ? "danger" : "warning"} dot>
            {healthText}
          </Badge>
          <span>版本 {health.data?.version || "--"}</span>
        </div>
        <div className="health-list">
          {(health.data?.dependencies || []).map((item) => (
            <div className="health-row" key={item.name}>
              <div>
                <strong>{item.name}</strong>
                {item.reason && <p>{item.reason}</p>}
              </div>
              <Badge tone={item.available === false ? "danger" : "success"} dot>{stateLabel(item.state)}</Badge>
            </div>
          ))}
        </div>
        <h3 className="section-title">运行态组件</h3>
        <div className="health-list">
          {runtimeComponents.map((component) => (
            <div className="health-row" key={component.name}>
              <div>
                <strong>{component.name}</strong>
                {component.reason && <p>{component.reason}</p>}
              </div>
              <Badge tone={component.available === false ? "warning" : "success"} dot>{stateLabel(component.state)}</Badge>
            </div>
          ))}
          {!runtimeComponents.length && <p className="muted">暂无运行态组件数据。</p>}
        </div>
      </Drawer>

      <Modal
        open={commandOpen}
        onClose={() => setCommandOpen(false)}
        title="快速跳转"
        subtitle="搜索页面或动作；不搜索业务数据"
        size="md"
      >
        <div className="command-input">
          <Search size={16} />
          <input autoFocus value={commandQuery} onChange={(event) => setCommandQuery(event.target.value)} placeholder="输入页面名，例如：策略、WAN、运行态" />
        </div>
        <div className="command-list">
          {commands.pages.map((page) => (
            <button type="button" key={page.key} onClick={() => runCommand(page.key, page.path)}>
              <span className="command-icon">{page.icon}</span>
              <span><strong>{page.label}</strong><small>{page.description}</small></span>
              <ChevronRight size={14} />
            </button>
          ))}
          {commands.actions.map((action) => (
            <button type="button" key={action.key} onClick={() => runCommand(action.key)}>
              <span className="command-icon">{action.icon}</span>
              <span><strong>{action.label}</strong><small>{action.description}</small></span>
              <ChevronRight size={14} />
            </button>
          ))}
          {!commands.pages.length && !commands.actions.length && <p className="muted">没有匹配的页面或动作。</p>}
        </div>
      </Modal>

      <Modal
        open={passwordOpen}
        onClose={() => !passwordBusy && setPasswordOpen(false)}
        title="修改密码"
        subtitle={session?.username}
        size="sm"
        busy={passwordBusy}
        footer={
          <>
            <Button onClick={() => setPasswordOpen(false)} disabled={passwordBusy}>取消</Button>
            <Button variant="primary" loading={passwordBusy} onClick={submitPassword}>保存</Button>
          </>
        }
      >
        <div className="form-stack">
          <Field label="当前密码"><Input type="password" autoComplete="current-password" value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} /></Field>
          <Field label="新密码" hint="至少 8 位，不能包含用户名"><Input type="password" autoComplete="new-password" value={newPassword} onChange={(event) => setNewPassword(event.target.value)} /></Field>
          <Field label="确认新密码"><Input type="password" autoComplete="new-password" value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} /></Field>
          {passwordError && <div className="form-error">{passwordError}</div>}
        </div>
      </Modal>
    </div>
  );
}
