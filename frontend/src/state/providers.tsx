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
import { afFallbackPorts, applyFailureMessage, humanErrorMessage, sanitizeDetail } from "../lib/human";
import { DependencyGuardProvider } from "./dependencies";

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

export type ApplyPhase = "idle" | "applying" | "done" | "unchanged" | "degraded" | "failed" | "consent";

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
  requestAfConsent: (ports: string[]) => void;
  consentAf: () => Promise<void>;
  declineAf: () => void;
  dismiss: () => void;
}

const ApplyContext = createContext<ApplyContextValue | null>(null);

function terminalStatus(status?: string) {
  const value = String(status || "").toLowerCase();
  return ["committed", "applied", "unchanged", "skipped", "partial", "degraded", "apply_failed", "failed", "unavailable", "dataplane_locked"].includes(value);
}

/**
 * The legacy `status=committed` no longer proves that commands were replayed.
 * Only `outcome` / generation evidence decides what the UI says.
 */
export function applyOutcomeOf(result?: RuntimeApply | null): string {
  const outcome = String(result?.outcome || "").toLowerCase();
  if (outcome) return outcome;
  if (result?.unchanged) return "unchanged";
  const status = String(result?.status || result?.runtime_state || "").toLowerCase();
  if (status === "skipped" || status === "unchanged") return "unchanged";
  if (result?.partial || status === "partial" || status === "degraded") return "partial";
  if (/fail|locked|error/.test(status)) return "failed";
  // `committed` alone is deliberately not promoted to "applied": only the
  // explicit outcome proves whether commands were re-sent.
  return "";
}

export function applyOutcomeLabel(outcome?: string): string {
  const value = String(outcome || "").toLowerCase();
  if (value === "unchanged") return "未变更";
  if (value === "partial") return "部分成功";
  if (value === "skipped") return "已跳过";
  if (value === "failed") return "失败";
  if (value === "applied") return "已应用";
  return "未提供 outcome";
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
    const outcome = applyOutcomeOf(next);
    const ports = afFallbackPorts(next);
    if (ports) {
      setAfFallback({ ports });
      setPhase("consent");
      setMessage("部分网卡无法启用高性能接管，需要确认接管方式");
      runningRef.current = false;
      return;
    }
    if (outcome === "failed" || status.includes("fail") || runtimeState.includes("fail") || status === "dataplane_locked") {
      const failure = applyFailureMessage(next);
      setPhase("failed");
      setMessage(failure);
      toast(failure, "error");
    } else if (outcome === "unchanged" || outcome === "skipped" || next.unchanged) {
      setPhase("unchanged");
      setMessage("配置未变更，本次未重新下发");
      toast("配置未变更，已跳过下发", "info");
    } else if (outcome === "partial" || next.partial) {
      const reasons = (next.degraded_reasons || []).map((reason) => sanitizeDetail(reason, "")).filter(Boolean);
      setPhase("degraded");
      setMessage(reasons.length ? `部分成功：${reasons.join("；")}` : "配置已应用，但存在部分降级步骤");
      toast("配置部分成功，请查看降级原因", "info");
    } else if (status === "degraded" || runtimeState === "degraded") {
      setPhase("degraded");
      setMessage("配置已应用，但组件处于降级状态");
    } else {
      setPhase("done");
      setMessage("");
    }
    runningRef.current = false;
    queryClient.invalidateQueries({ queryKey: ["runtime"] });
    queryClient.invalidateQueries({ queryKey: ["runtime", "apply-state"] });
    queryClient.invalidateQueries({ queryKey: ["config", "dependencies"] });
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
    setMessage("");
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
      if (response.outcome || terminalStatus(response.status || response.runtime_state)) {
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

  const requestAfConsent = useCallback((ports: string[]) => {
    // Retained for callers that still surface the legacy apply gate. The
    // compatibility path is automatic on the backend now, so no consent is
    // stored; the next apply retries with the automatic fallback.
    setAfFallback({ ports: ports.filter(Boolean) });
    setPhase("consent");
    setMessage("系统会自动回落到兼容模式，正在重新应用");
    runningRef.current = false;
    void applyRef.current?.();
  }, []);

  const consentAf = useCallback(async () => {
    setConsentBusy(true);
    try {
      setAfFallback(null);
      toast("系统会自动使用兼容模式，正在重新应用", "info");
      runningRef.current = false;
      await apply();
    } catch (error) {
      const failure = humanErrorMessage(error, "重新应用失败");
      setPhase("failed");
      setMessage(failure);
      toast(failure, "error");
    } finally {
      setConsentBusy(false);
    }
  }, [apply, toast]);

  const declineAf = useCallback(() => {
    setAfFallback(null);
    setPhase("idle");
    setMessage("");
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
      requestAfConsent,
      consentAf,
      declineAf,
      dismiss,
    }),
    [phase, message, transactionId, startedAt, result, afFallback, consentBusy, apply, requestAfConsent, consentAf, declineAf, dismiss]
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
          <DependencyGuardProvider>
            <ApplyProvider>{children}</ApplyProvider>
          </DependencyGuardProvider>
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
      title="系统将自动使用兼容模式"
      subtitle="无需手工确认，性能会受限"
      size="sm"
      busy={consentBusy}
      footer={
        <>
          <Button onClick={declineAf} disabled={consentBusy}>关闭</Button>
          <Button variant="primary" loading={consentBusy} onClick={() => void consentAf()}>重新应用</Button>
        </>
      }
    >
      <div className="form-stack">
        <p>以下网卡的接管方式由系统自动决定；无法使用高性能接管时会自动回落到兼容模式，吞吐会下降、CPU 占用会升高，其余网卡不受影响。</p>
        <div className="notice-block">
          <strong>受影响的网卡</strong>
          <span>{afFallback?.ports.length ? afFallback.ports.join("、") : "当前网卡"}</span>
        </div>
        <p className="muted">接管方式不可手工选择；界面只展示实际状态和原因。</p>
      </div>
    </Modal>
  );
  return modal;
}
