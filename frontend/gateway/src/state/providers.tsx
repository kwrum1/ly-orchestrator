import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { useQueryClient } from "@tanstack/react-query";
import { API } from "../api/endpoints";
import { ApiError, getJson, postJson } from "../api/client";
import { queryClient } from "../api/queryClient";
import { useRuntime, useSession } from "../api/hooks";
import type { RuntimeApply, SessionResponse, SessionUser } from "../api/types";
import { Button, Modal, ToastProvider, useToast } from "../components/ui";
import { formatDateTime } from "../lib/format";

interface AuthContextValue {
  session: SessionUser | null;
  loading: boolean;
  error: unknown;
  login: (username: string, password: string) => Promise<{ passwordChangeRequired: boolean }>;
  changePassword: (currentPassword: string, newPassword: string) => Promise<void>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const query = useSession();
  const [session, setSession] = useState<SessionUser | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    if (query.data?.session) {
      setSession(query.data.session);
      setError(null);
      setLoading(false);
    } else if (query.isError) {
      setSession(null);
      setError(query.error);
      setLoading(false);
    } else if (!query.isLoading) {
      setSession(null);
      setLoading(false);
    }
    const timer = window.setTimeout(() => setLoading(false), 2500);
    return () => window.clearTimeout(timer);
  }, [query.data, query.isError, query.error, query.isLoading]);

  useEffect(() => {
    const onUnauthorized = () => {
      setSession(null);
      queryClient.removeQueries({ queryKey: ["session"] });
    };
    window.addEventListener("lyroute:unauthorized", onUnauthorized);
    return () => window.removeEventListener("lyroute:unauthorized", onUnauthorized);
  }, []);

  const login = useCallback(async (username: string, password: string) => {
    const result = await postJson<SessionResponse>(API.auth.login, { username, password });
    queryClient.setQueryData(["session"], result);
    setSession(result.session);
    setError(null);
    return { passwordChangeRequired: Boolean(result.password_change_required || result.session?.password_change_required) };
  }, []);

  const changePassword = useCallback(async (currentPassword: string, newPassword: string) => {
    await postJson(API.auth.changePassword, { current_password: currentPassword, new_password: newPassword });
    queryClient.invalidateQueries({ queryKey: ["session"] });
    const next = await getJson<SessionResponse>(API.auth.session);
    queryClient.setQueryData(["session"], next);
    setSession(next.session);
  }, []);

  const logout = useCallback(async () => {
    try {
      await postJson(API.auth.logout, {});
    } finally {
      queryClient.clear();
      setSession(null);
    }
  }, []);

  const refresh = useCallback(async () => {
    const next = await getJson<SessionResponse>(API.auth.session);
    queryClient.setQueryData(["session"], next);
    setSession(next.session);
  }, []);

  const value = useMemo(
    () => ({ session, loading, error, login, changePassword, logout, refresh }),
    [session, loading, error, login, changePassword, logout, refresh]
  );
  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth() {
  const context = useContext(AuthContext);
  if (!context) throw new Error("useAuth must be used inside AuthProvider");
  return context;
}

interface ConfirmOptions {
  title: string;
  message: ReactNode;
  detail?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  danger?: boolean;
}

interface ConfirmContextValue {
  confirm: (options: ConfirmOptions) => Promise<boolean>;
}

const ConfirmContext = createContext<ConfirmContextValue | null>(null);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [options, setOptions] = useState<ConfirmOptions | null>(null);
  const resolver = useRef<((value: boolean) => void) | null>(null);

  const confirm = useCallback((next: ConfirmOptions) => {
    return new Promise<boolean>((resolve) => {
      resolver.current = resolve;
      setOptions(next);
    });
  }, []);

  const close = (value: boolean) => {
    resolver.current?.(value);
    resolver.current = null;
    setOptions(null);
  };

  return (
    <ConfirmContext.Provider value={{ confirm }}>
      {children}
      <Modal
        open={Boolean(options)}
        title={options?.title || ""}
        size="sm"
        onClose={() => close(false)}
        footer={
          <>
            <Button onClick={() => close(false)}>{options?.cancelLabel || "取消"}</Button>
            <Button variant={options?.danger ? "danger" : "primary"} onClick={() => close(true)}>
              {options?.confirmLabel || "确认"}
            </Button>
          </>
        }
      >
        <div className="confirm-body">
          <p>{options?.message}</p>
          {options?.detail && <div className="confirm-detail">{options.detail}</div>}
        </div>
      </Modal>
    </ConfirmContext.Provider>
  );
}

export function useConfirm() {
  const context = useContext(ConfirmContext);
  if (!context) throw new Error("useConfirm must be used inside ConfirmProvider");
  return context.confirm;
}

export type ApplyPhase = "idle" | "applying" | "done" | "degraded" | "failed";

interface ApplyContextValue {
  phase: ApplyPhase;
  running: boolean;
  message: string;
  transactionId: string;
  startedAt: number;
  result: RuntimeApply | null;
  apply: () => Promise<RuntimeApply | null>;
  dismiss: () => void;
}

const ApplyContext = createContext<ApplyContextValue | null>(null);

function terminalStatus(status?: string) {
  const value = String(status || "").toLowerCase();
  return ["committed", "applied", "degraded", "apply_failed", "failed", "unavailable", "dataplane_locked"].includes(value);
}

export function ApplyProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const runtime = useRuntime();
  const [phase, setPhase] = useState<ApplyPhase>("idle");
  const [message, setMessage] = useState("");
  const [transactionId, setTransactionId] = useState("");
  const [startedAt, setStartedAt] = useState(0);
  const [result, setResult] = useState<RuntimeApply | null>(null);
  const runningRef = useRef(false);

  const finish = useCallback((next: RuntimeApply) => {
    setResult(next);
    const status = String(next.status || next.runtime_state || "").toLowerCase();
    const runtimeState = String(next.runtime_state || "").toLowerCase();
    if (status.includes("fail") || runtimeState.includes("fail") || status === "dataplane_locked") {
      setPhase("failed");
      setMessage(next.reason || "应用失败");
      toast(next.reason || "运行态应用失败", "error");
    } else if (status === "degraded" || runtimeState === "degraded") {
      setPhase("degraded");
      setMessage(next.reason || "应用完成，但存在降级组件");
      toast("应用完成，部分组件降级", "info");
    } else {
      setPhase("done");
      setMessage("运行态应用成功");
      toast("运行态应用成功", "success");
    }
    runningRef.current = false;
    queryClient.invalidateQueries({ queryKey: ["runtime"] });
    queryClient.invalidateQueries({ queryKey: ["resources"] });
    queryClient.invalidateQueries({ queryKey: ["telemetry"] });
  }, [queryClient, toast]);

  const poll = useCallback(async (initial?: RuntimeApply) => {
    const deadline = Date.now() + 12 * 60_000;
    let latest = initial;
    while (Date.now() < deadline) {
      await new Promise((resolve) => window.setTimeout(resolve, 3500));
      try {
        const status = await getJson<{ last_apply?: RuntimeApply; status?: string }>(API.runtime.status, 25_000);
        latest = status.last_apply || latest;
        if (latest?.transaction_id && latest.transaction_id !== transactionId) {
          // The backend may have completed a different transaction; keep waiting
          // until the status shows a terminal state.
        }
        if (latest && terminalStatus(latest.status || latest.runtime_state)) {
          finish(latest);
          return latest;
        }
        if (status.status === "degraded" && latest && terminalStatus(latest.status)) {
          finish(latest);
          return latest;
        }
      } catch {
        // A slow status endpoint is expected; keep the transaction visible.
      }
    }
    setPhase("failed");
    setMessage("应用状态确认超时，请到运行态页面查看结果");
    runningRef.current = false;
    return latest || null;
  }, [finish, transactionId]);

  const apply = useCallback(async () => {
    if (runningRef.current) return null;
    runningRef.current = true;
    setPhase("applying");
    setMessage("正在应用运行态，请勿关闭页面");
    setStartedAt(Date.now());
    setResult(null);
    try {
      const response = await postJson<RuntimeApply>(API.runtime.apply, {}, 620_000);
      setTransactionId(response.transaction_id || "");
      if (response.status === "apply_in_progress") {
        setMessage("已有应用在执行，正在等待结果");
        return await poll(response);
      }
      if (response.status === "dataplane_locked") {
        finish(response);
        return response;
      }
      if (terminalStatus(response.status || response.runtime_state)) {
        finish(response);
        return response;
      }
      return await poll(response);
    } catch (error) {
      const apiError = error as ApiError;
      setPhase("failed");
      setMessage(apiError.message || "应用请求失败");
      runningRef.current = false;
      toast(apiError.message || "应用请求失败", "error", apiError.requestId);
      return null;
    }
  }, [finish, poll, toast]);

  const dismiss = useCallback(() => {
    if (runningRef.current) return;
    setPhase("idle");
    setResult(null);
    setMessage("");
    setTransactionId("");
  }, []);

  useEffect(() => {
    const last = runtime.data?.last_apply;
    if (last && phase === "idle") {
      setResult(last);
      setTransactionId(last.transaction_id || "");
      const status = String(last.status || last.runtime_state || "").toLowerCase();
      if (status.includes("fail")) setPhase("failed");
      else if (status === "degraded") setPhase("degraded");
      else if (terminalStatus(status)) setPhase("done");
    }
  }, [runtime.data, phase]);

  const value = useMemo(
    () => ({
      phase,
      running: phase === "applying",
      message,
      transactionId,
      startedAt,
      result,
      apply,
      dismiss,
    }),
    [phase, message, transactionId, startedAt, result, apply, dismiss]
  );
  return <ApplyContext.Provider value={value}>{children}</ApplyContext.Provider>;
}

export function useApply() {
  const context = useContext(ApplyContext);
  if (!context) throw new Error("useApply must be used inside ApplyProvider");
  return context;
}

export function AppProviders({ children }: { children: ReactNode }) {
  return (
    <ToastProvider>
      <AuthProvider>
        <ConfirmProvider>
          <ApplyProvider>{children}</ApplyProvider>
        </ConfirmProvider>
      </AuthProvider>
    </ToastProvider>
  );
}

export function ApplyBanner() {
  const { phase, message, transactionId, startedAt, result, dismiss } = useApply();
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (phase !== "applying") return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [phase]);
  if (phase === "idle") return null;
  const elapsed = startedAt ? Math.max(0, Math.round((now - startedAt) / 1000)) : 0;
  const tone = phase === "failed" ? "danger" : phase === "degraded" ? "warning" : phase === "done" ? "success" : "info";
  return (
    <div className={`apply-banner apply-${tone}`}>
      <div>
        <strong>{phase === "applying" ? "正在应用运行态" : phase === "done" ? "运行态已应用" : phase === "degraded" ? "运行态部分降级" : "运行态应用失败"}</strong>
        <span>{message}{transactionId ? ` · 事务 ${transactionId}` : ""}{startedAt ? ` · 已用 ${elapsed}s` : ""}</span>
        {result?.applied_at && <em>完成于 {formatDateTime(result.applied_at)}</em>}
      </div>
      {phase !== "applying" && <Button size="sm" variant="ghost" onClick={dismiss}>关闭</Button>}
    </div>
  );
}
