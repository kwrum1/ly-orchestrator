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
import { setAfFallbackConsent } from "../api/dataplane";
import type { RuntimeApply, SessionResponse, SessionUser } from "../api/types";
import { Button, Modal, ToastProvider, useToast } from "../components/ui";
import { formatDateTime } from "../lib/format";
import { afFallbackPorts, applyFailureMessage, humanErrorMessage } from "../lib/human";

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

export type ApplyPhase = "idle" | "applying" | "done" | "degraded" | "failed" | "consent";

interface ApplyContextValue {
  phase: ApplyPhase;
  running: boolean;
  message: string;
  transactionId: string;
  startedAt: number;
  result: RuntimeApply | null;
  afFallback: { ports: string[] } | null;
  consentBusy: boolean;
  apply: () => Promise<RuntimeApply | null>;
  consentAf: () => Promise<void>;
  declineAf: () => void;
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
  const [afFallback, setAfFallback] = useState<{ ports: string[] } | null>(null);
  const [consentBusy, setConsentBusy] = useState(false);
  const runningRef = useRef(false);
  const pendingRef = useRef(false);
  const applyRef = useRef<(() => Promise<RuntimeApply | null>) | null>(null);

  const triggerPending = useCallback(() => {
    if (!pendingRef.current || runningRef.current) return;
    pendingRef.current = false;
    window.setTimeout(() => {
      void applyRef.current?.();
    }, 0);
  }, []);

  const finish = useCallback((next: RuntimeApply) => {
    setResult(next);
    const status = String(next.status || next.runtime_state || "").toLowerCase();
    const runtimeState = String(next.runtime_state || "").toLowerCase();
    const ports = afFallbackPorts(next);
    if (ports) {
      setAfFallback({ ports });
      setPhase("consent");
      setMessage("部分网卡无法启用高性能接管，需要确认接管方式");
      runningRef.current = false;
      return;
    }
    if (status.includes("fail") || runtimeState.includes("fail") || status === "dataplane_locked") {
      const failure = applyFailureMessage(next);
      setPhase("failed");
      setMessage(failure);
      toast(failure, "error");
    } else if (status === "degraded" || runtimeState === "degraded") {
      setPhase("degraded");
      setMessage("部分配置未生效，请检查设备状态");
    } else {
      setPhase("done");
      setMessage("配置已生效");
    }
    runningRef.current = false;
    queryClient.invalidateQueries({ queryKey: ["runtime"] });
    queryClient.invalidateQueries({ queryKey: ["resources"] });
    queryClient.invalidateQueries({ queryKey: ["telemetry"] });
    triggerPending();
  }, [queryClient, toast, triggerPending]);

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
    setMessage("应用状态确认超时，请稍后刷新查看");
    runningRef.current = false;
    triggerPending();
    return latest || null;
  }, [finish, transactionId, triggerPending]);

  const apply = useCallback(async () => {
    if (runningRef.current) {
      pendingRef.current = true;
      setPhase("applying");
      setMessage("已保存，等待当前应用结束后自动应用");
      return null;
    }
    runningRef.current = true;
    setPhase("applying");
    setMessage("正在应用配置，请勿关闭页面");
    setStartedAt(Date.now());
    setResult(null);
    try {
      const response = await postJson<RuntimeApply>(API.runtime.apply, {}, 620_000);
      setTransactionId(response.transaction_id || "");
      if (afFallbackPorts(response)) {
        finish(response);
        return response;
      }
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
      if (afFallbackPorts(apiError)) {
        finish({ status: "af_fallback_required", ...(apiError.body as any) } as RuntimeApply);
        return null;
      }
      const failure = humanErrorMessage(apiError?.message, "应用请求失败，请稍后重试");
      setPhase("failed");
      setMessage(failure);
      runningRef.current = false;
      toast(failure, "error");
      triggerPending();
      return null;
    }
  }, [finish, poll, toast, triggerPending]);
  applyRef.current = apply;

  const consentAf = useCallback(async () => {
    const ports = afFallback?.ports || [];
    setConsentBusy(true);
    try {
      await setAfFallbackConsent(true, ports);
      setAfFallback(null);
      await queryClient.invalidateQueries({ queryKey: ["dataplane", "policy"] });
      await queryClient.invalidateQueries({ queryKey: ["resources"] });
      await queryClient.invalidateQueries({ queryKey: ["telemetry"] });
      toast("已启用兼容模式，正在重新应用", "info");
      runningRef.current = false;
      await apply();
    } catch (error) {
      const failure = humanErrorMessage(error, "启用兼容模式失败");
      setPhase("failed");
      setMessage(failure);
      toast(failure, "error");
    } finally {
      setConsentBusy(false);
    }
  }, [afFallback, apply, queryClient, toast]);

  const declineAf = useCallback(() => {
    setAfFallback(null);
    setPhase("idle");
    setMessage("");
    toast("已保持高性能接管，未启用兼容模式", "info");
  }, [toast]);

  const dismiss = useCallback(() => {
    if (runningRef.current) return;
    setPhase("idle");
    setResult(null);
    setMessage("");
    setTransactionId("");
  }, []);

  useEffect(() => {
    const last = runtime.data?.last_apply;
    if (!last) return;
    setResult((current) => current || last);
    setTransactionId((current) => current || last.transaction_id || "");
  }, [runtime.data]);

  const value = useMemo(
    () => ({
      phase,
      running: phase === "applying",
      message,
      transactionId,
      startedAt,
      result,
      afFallback,
      consentBusy,
      apply,
      consentAf,
      declineAf,
      dismiss,
    }),
    [phase, message, transactionId, startedAt, result, afFallback, consentBusy, apply, consentAf, declineAf, dismiss]
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
  const { phase, message, startedAt, result, afFallback, consentBusy, consentAf, declineAf, dismiss } = useApply();
  const [now, setNow] = useState(Date.now());
  useEffect(() => {
    if (phase !== "applying") return;
    const timer = window.setInterval(() => setNow(Date.now()), 1000);
    return () => window.clearInterval(timer);
  }, [phase]);
  const modal = (
    <Modal
      open={Boolean(afFallback)}
      onClose={() => !consentBusy && declineAf()}
      title="该网卡无法启用高性能接管"
      subtitle="可以改用兼容模式，但性能会受限"
      size="sm"
      busy={consentBusy}
      footer={
        <>
          <Button onClick={declineAf} disabled={consentBusy}>暂不启用</Button>
          <Button variant="primary" loading={consentBusy} onClick={() => void consentAf()}>启用兼容模式</Button>
        </>
      }
    >
      <div className="form-stack">
        <p>以下网卡无法使用高性能接管方式。启用兼容模式后，这些网卡的吞吐会下降，CPU 占用会升高；其余网卡不受影响。</p>
        <div className="notice-block">
          <strong>受影响的网卡</strong>
          <span>{afFallback?.ports.length ? afFallback.ports.join("、") : "当前网卡"}</span>
        </div>
        <p className="muted">启用后可以随时在“网卡设置”中关闭兼容模式。</p>
      </div>
    </Modal>
  );
  if (phase === "idle") return modal;
  const elapsed = startedAt ? Math.max(0, Math.round((now - startedAt) / 1000)) : 0;
  const tone = phase === "failed" ? "danger" : phase === "degraded" || phase === "consent" ? "warning" : phase === "done" ? "success" : "info";
  const title = phase === "applying"
    ? "正在应用配置"
    : phase === "done"
      ? "配置已生效"
      : phase === "degraded"
        ? "部分配置未生效"
        : phase === "consent"
          ? "等待确认网卡接管方式"
          : "配置应用失败";
  return (
    <>
      {modal}
      <div className={`apply-banner apply-${tone}`}>
        <div>
          <strong>{title}</strong>
          <span>{message}{startedAt ? ` · 已用 ${elapsed}s` : ""}</span>
          {result?.applied_at && <em>完成于 {formatDateTime(result.applied_at)}</em>}
        </div>
        {phase !== "applying" && <Button size="sm" variant="ghost" onClick={dismiss}>关闭</Button>}
      </div>
    </>
  );
}
