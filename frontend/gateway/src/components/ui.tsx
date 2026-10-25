import {
  createContext,
  useContext,
  useEffect,
  useId,
  useMemo,
  useRef,
  useState,
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { createPortal } from "react-dom";
import { AlertCircle, Check, Copy, Loader2, X } from "lucide-react";
import { classNames } from "../lib/format";

export function Button({
  variant = "secondary",
  size = "md",
  loading,
  icon,
  children,
  className,
  ...props
}: ButtonHTMLAttributes<HTMLButtonElement> & {
  variant?: "primary" | "secondary" | "ghost" | "danger" | "text";
  size?: "sm" | "md";
  loading?: boolean;
  icon?: ReactNode;
}) {
  return (
    <button
      {...props}
      className={classNames("btn", `btn-${variant}`, size === "sm" && "btn-sm", className)}
      disabled={props.disabled || loading}
    >
      {loading ? <Loader2 size={14} className="spin" /> : icon}
      {children}
    </button>
  );
}

export function IconButton({ label, children, ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { label: string }) {
  return (
    <button {...props} className={classNames("icon-btn", props.className)} aria-label={label} title={label}>
      {children}
    </button>
  );
}

export function Badge({
  tone = "neutral",
  dot,
  children,
  title,
}: {
  tone?: "neutral" | "success" | "warning" | "danger" | "info" | "violet";
  dot?: boolean;
  children: ReactNode;
  title?: string;
}) {
  return (
    <span className={`badge badge-${tone}`} title={title}>
      {dot && <i className="status-dot" />}
      {children}
    </span>
  );
}

export function Card({
  title,
  subtitle,
  actions,
  children,
  className,
  bodyClassName,
}: {
  title?: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  children: ReactNode;
  className?: string;
  bodyClassName?: string;
}) {
  return (
    <section className={classNames("card", className)}>
      {(title || actions) && (
        <header className="card-head">
          <div className="card-title">
            <h2>{title}</h2>
            {subtitle && <p>{subtitle}</p>}
          </div>
          {actions && <div className="card-actions">{actions}</div>}
        </header>
      )}
      <div className={classNames("card-body", bodyClassName)}>{children}</div>
    </section>
  );
}

export function PageHeader({
  title,
  subtitle,
  actions,
  meta,
}: {
  title: ReactNode;
  subtitle?: ReactNode;
  actions?: ReactNode;
  meta?: ReactNode;
}) {
  return (
    <div className="page-head">
      <div>
        <h1>{title}</h1>
        {subtitle && <p>{subtitle}</p>}
        {meta && <div className="page-meta">{meta}</div>}
      </div>
      {actions && <div className="page-actions">{actions}</div>}
    </div>
  );
}

export function Toolbar({ left, right }: { left?: ReactNode; right?: ReactNode }) {
  return (
    <div className="toolbar">
      <div className="toolbar-left">{left}</div>
      <div className="toolbar-right">{right}</div>
    </div>
  );
}

export function EmptyState({
  title,
  description,
  action,
  icon,
}: {
  title: string;
  description?: string;
  action?: ReactNode;
  icon?: ReactNode;
}) {
  return (
    <div className="empty-state">
      <div className="empty-icon">{icon || <AlertCircle size={22} />}</div>
      <strong>{title}</strong>
      {description && <p>{description}</p>}
      {action && <div className="empty-action">{action}</div>}
    </div>
  );
}

export function ErrorState({ error, onRetry }: { error: unknown; onRetry?: () => void }) {
  const message = error instanceof Error ? error.message : String(error || "未知错误");
  return (
    <div className="error-state">
      <AlertCircle size={20} />
      <div>
        <strong>数据读取失败</strong>
        <p>{message}</p>
      </div>
      {onRetry && <Button size="sm" onClick={onRetry}>重试</Button>}
    </div>
  );
}

export function Skeleton({ rows = 5, className }: { rows?: number; className?: string }) {
  return (
    <div className={classNames("skeleton-list", className)} aria-label="加载中">
      {Array.from({ length: rows }).map((_, index) => (
        <div className="skeleton-row" key={index}>
          <span />
          <span />
          <span />
        </div>
      ))}
    </div>
  );
}

export function Spinner({ label }: { label?: string }) {
  return (
    <span className="spinner">
      <Loader2 size={14} className="spin" />
      {label}
    </span>
  );
}

export function Field({
  label,
  required,
  hint,
  error,
  children,
  className,
}: {
  label: ReactNode;
  required?: boolean;
  hint?: ReactNode;
  error?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <label className={classNames("field", error ? "has-error" : "", className)}>
      <span className="field-label">
        {label}
        {required && <em>*</em>}
      </span>
      {children}
      {error && <span className="field-error">{error}</span>}
      {!error && hint && <span className="field-hint">{hint}</span>}
    </label>
  );
}

export function Input(props: InputHTMLAttributes<HTMLInputElement>) {
  return <input {...props} className={classNames("input", props.className)} />;
}

export function Select(props: SelectHTMLAttributes<HTMLSelectElement>) {
  return <select {...props} className={classNames("select", props.className)} />;
}

export function Textarea(props: TextareaHTMLAttributes<HTMLTextAreaElement>) {
  return <textarea {...props} className={classNames("textarea", props.className)} />;
}

export function Switch({
  checked,
  onChange,
  label,
  disabled,
}: {
  checked: boolean;
  onChange: (checked: boolean) => void;
  label?: ReactNode;
  disabled?: boolean;
}) {
  return (
    <label className={classNames("switch-row", disabled && "is-disabled")}>
      <button
        type="button"
        role="switch"
        aria-checked={checked}
        className={classNames("switch", checked && "is-on")}
        onClick={() => !disabled && onChange(!checked)}
        disabled={disabled}
      >
        <i />
      </button>
      {label && <span>{label}</span>}
    </label>
  );
}

export function Tabs({
  items,
  value,
  onChange,
}: {
  items: { key: string; label: ReactNode; count?: number }[];
  value: string;
  onChange: (key: string) => void;
}) {
  return (
    <div className="tabs" role="tablist">
      {items.map((item) => (
        <button
          key={item.key}
          type="button"
          role="tab"
          aria-selected={item.key === value}
          className={classNames("tab", item.key === value && "is-active")}
          onClick={() => onChange(item.key)}
        >
          {item.label}
          {item.count !== undefined && <span>{item.count}</span>}
        </button>
      ))}
    </div>
  );
}

export function Modal({
  open,
  title,
  subtitle,
  children,
  footer,
  onClose,
  size = "md",
  busy,
}: {
  open: boolean;
  title: ReactNode;
  subtitle?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  onClose: () => void;
  size?: "sm" | "md" | "lg" | "xl";
  busy?: boolean;
}) {
  const titleId = useId();
  const panelRef = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);
  const busyRef = useRef(busy);
  onCloseRef.current = onClose;
  busyRef.current = busy;
  // Focus and key handling must run only when the dialog opens/closes. Depending
  // on the inline onClose prop re-ran this effect after every keystroke, which
  // restored focus outside the dialog and then jumped it to the close button.
  useEffect(() => {
    if (!open) return;
    const previous = document.activeElement as HTMLElement | null;
    const panel = panelRef.current;
    const focusable = () => panel?.querySelectorAll<HTMLElement>(
      'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
    ) || [];
    const timer = window.setTimeout(() => {
      if (panel?.contains(document.activeElement)) return;
      const first = panel?.querySelector<HTMLElement>("input:not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled])");
      first?.focus();
    }, 30);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !busyRef.current) {
        event.preventDefault();
        onCloseRef.current();
      }
      if (event.key !== "Tab") return;
      const elements = Array.from(focusable()).filter((element) => element.offsetParent !== null);
      if (!elements.length) return;
      const first = elements[0];
      const last = elements[elements.length - 1];
      if (event.shiftKey && document.activeElement === first) {
        event.preventDefault();
        last.focus();
      } else if (!event.shiftKey && document.activeElement === last) {
        event.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKeyDown);
    document.body.classList.add("modal-open");
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("keydown", onKeyDown);
      document.body.classList.remove("modal-open");
      previous?.focus?.();
    };
  }, [open]);

  if (!open) return null;
  return createPortal(
    <div className="modal-backdrop" onMouseDown={(event) => event.target === event.currentTarget && !busy && onClose()}>
      <div ref={panelRef} className={`modal modal-${size}`} role="dialog" aria-modal="true" aria-labelledby={titleId}>
        <header className="modal-head">
          <div>
            <h2 id={titleId}>{title}</h2>
            {subtitle && <p>{subtitle}</p>}
          </div>
          <IconButton label="关闭" onClick={() => !busy && onClose()} disabled={busy}><X size={18} /></IconButton>
        </header>
        <div className="modal-body">{children}</div>
        {footer && <footer className="modal-footer">{footer}</footer>}
      </div>
    </div>,
    document.body
  );
}

export function Drawer({
  open,
  title,
  subtitle,
  children,
  footer,
  onClose,
  width = "md",
}: {
  open: boolean;
  title: ReactNode;
  subtitle?: ReactNode;
  children: ReactNode;
  footer?: ReactNode;
  onClose: () => void;
  width?: "sm" | "md" | "lg";
}) {
  const titleId = useId();
  const panelRef = useRef<HTMLDivElement>(null);
  const onCloseRef = useRef(onClose);
  onCloseRef.current = onClose;
  useEffect(() => {
    if (!open) return;
    const previous = document.activeElement as HTMLElement | null;
    const panel = panelRef.current;
    const timer = window.setTimeout(() => {
      if (panel?.contains(document.activeElement)) return;
      panel?.querySelector<HTMLElement>("input:not([disabled]), select:not([disabled]), textarea:not([disabled]), button:not([disabled])")?.focus();
    }, 30);
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        event.preventDefault();
        onCloseRef.current();
      }
    };
    document.addEventListener("keydown", onKeyDown);
    document.body.classList.add("drawer-open");
    return () => {
      window.clearTimeout(timer);
      document.removeEventListener("keydown", onKeyDown);
      document.body.classList.remove("drawer-open");
      previous?.focus?.();
    };
  }, [open]);
  if (!open) return null;
  return createPortal(
    <div className="drawer-backdrop" onMouseDown={(event) => event.target === event.currentTarget && onClose()}>
      <aside ref={panelRef} className={`drawer drawer-${width}`} role="dialog" aria-modal="true" aria-labelledby={titleId}>
        <header className="drawer-head">
          <div>
            <h2 id={titleId}>{title}</h2>
            {subtitle && <p>{subtitle}</p>}
          </div>
          <IconButton label="关闭" onClick={onClose}><X size={18} /></IconButton>
        </header>
        <div className="drawer-body">{children}</div>
        {footer && <footer className="drawer-footer">{footer}</footer>}
      </aside>
    </div>,
    document.body
  );
}

export function StatusDot({ state }: { state?: string }) {
  const value = String(state || "").toLowerCase();
  const tone = ["running", "available", "applied", "healthy", "ok", "online", "up", "reachable"].includes(value)
    ? "success"
    : ["degraded", "pending", "desired_not_applied", "stale", "warning"].includes(value)
      ? "warning"
      : ["failed", "unavailable", "offline", "down", "error"].includes(value)
        ? "danger"
        : "neutral";
  return <i className={`status-dot status-dot-${tone}`} />;
}

export function CopyText({ value, display, className }: { value: string; display?: ReactNode; className?: string }) {
  const [copied, setCopied] = useState(false);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(value);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 1200);
    } catch {
      // Clipboard may be unavailable on plain HTTP; keep the value selectable.
    }
  };
  return (
    <button type="button" className={classNames("copy-text", className)} onClick={copy} title={`复制 ${value}`}>
      {display ?? value}
      {copied ? <Check size={12} /> : <Copy size={12} />}
    </button>
  );
}

export function Metric({
  label,
  value,
  hint,
  tone = "blue",
  icon,
  spark,
}: {
  label: string;
  value: ReactNode;
  hint?: ReactNode;
  tone?: "blue" | "violet" | "amber" | "green" | "plain";
  icon?: ReactNode;
  spark?: ReactNode;
}) {
  return (
    <article className={`metric metric-${tone}`}>
      <div className="metric-top">
        <span>{label}</span>
        {icon && <span className="metric-icon">{icon}</span>}
      </div>
      <strong>{value}</strong>
      <div className="metric-bottom">
        {hint && <em>{hint}</em>}
        {spark}
      </div>
    </article>
  );
}

export function ProgressBar({ value, tone = "blue" }: { value: number; tone?: "blue" | "green" | "amber" | "red" }) {
  return (
    <div className={`progress progress-${tone}`} role="progressbar" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(value)}>
      <i style={{ width: `${Math.max(0, Math.min(100, value))}%` }} />
    </div>
  );
}

export function useToast() {
  return useContext(ToastContext);
}

export type ToastKind = "success" | "error" | "info";
export interface ToastItem { id: number; message: string; kind: ToastKind; requestId?: string }
export const ToastContext = createContext<{
  toast: (message: string, kind?: ToastKind, requestId?: string) => void;
}>({ toast: () => undefined });

export function ToastProvider({ children }: { children: ReactNode }) {
  const [items, setItems] = useState<ToastItem[]>([]);
  const idRef = useRef(1);
  const toast = (message: string, kind: ToastKind = "info", requestId?: string) => {
    const id = idRef.current++;
    setItems((current) => [...current, { id, message, kind, requestId }]);
    window.setTimeout(() => setItems((current) => current.filter((item) => item.id !== id)), 4000);
  };
  return (
    <ToastContext.Provider value={{ toast }}>
      {children}
      <div className="toast-stack" aria-live="polite">
        {items.map((item) => (
          <div className={`toast toast-${item.kind}`} key={item.id}>
            <span>{item.message}</span>
          </div>
        ))}
      </div>
    </ToastContext.Provider>
  );
}
