import { createContext, useCallback, useContext, useMemo, useState, type ReactNode } from "react";
import { AlertTriangle, Info } from "lucide-react";
import { dependencyConflictOf, displayErrorMessage, type DependencyConflict, type DependencyReference } from "../api/errors";
import { Badge, Button, Modal, useToast } from "../components/ui";

const TYPE_LABELS: Record<string, string> = {
  interface: "接口",
  interface_bond: "端口聚合",
  management_network: "管理网络",
  wan_link: "WAN 线路",
  wan_group: "WAN 组",
  object_group: "IP 组",
  domain_ip_set: "域名集",
  port_map: "端口映射",
  nat_static: "静态 NAT",
  route_policy: "策略路由",
  proxy_node: "代理节点",
  proxy_group: "代理组",
  proxy_subscription: "代理订阅",
  proxy_egress: "代理出口",
  dns_upstream: "DNS 上游",
  dns_policy: "DNS 策略",
  dhcp_server: "DHCP 服务",
  dhcp_static_binding: "DHCP 静态绑定",
  security_acl: "安全 ACL",
  security_ip_mac_binding: "IP/MAC 绑定",
  security_threat_intel: "威胁情报",
  security_attack_rule: "攻击防护",
  traffic_control: "流量控制",
};

function typeLabel(type?: string): string {
  const key = String(type || "");
  return TYPE_LABELS[key] || key || "对象";
}

export function dependencyObjectLabel(reference: DependencyReference): string {
  return reference.name || reference.id || "--";
}

function ReferenceTable({ title, items, tone }: { title: string; items: DependencyReference[]; tone?: "danger" | "warning" }) {
  if (!items.length) return null;
  return (
    <div className="dependency-section">
      <div className="dependency-section-head">
        <strong>{title}</strong>
        <Badge tone={tone || "warning"}>{items.length} 项</Badge>
      </div>
      <div className="mini-table-wrap">
        <table className="data-table">
          <thead>
            <tr><th>类型</th><th>对象</th><th>引用字段</th><th>建议</th></tr>
          </thead>
          <tbody>
            {items.map((item, index) => (
              <tr key={`${item.type}-${item.id}-${item.field || ""}-${index}`}>
                <td>{typeLabel(item.type)}</td>
                <td><strong>{dependencyObjectLabel(item)}</strong><small className="mono dependency-id">{item.id}</small></td>
                <td className="mono">{item.field || (item.depth ? `深度 ${item.depth}` : "--")}</td>
                <td className="muted">{item.suggestion || "修改或删除该引用者后重试"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

interface GuardOptions {
  fallback?: string;
  successMessage?: string;
  onSuccess?: (result: any) => void | Promise<void>;
  /** Return true when the caller rendered the error itself (field-level). */
  onError?: (error: unknown) => boolean;
}

interface PendingAction {
  conflict: DependencyConflict;
  action: (force: boolean) => Promise<unknown>;
  options: GuardOptions;
}

interface DependencyGuardValue {
  run: <T>(action: (force: boolean) => Promise<T>, options?: GuardOptions) => Promise<T | undefined>;
}

const DependencyGuardContext = createContext<DependencyGuardValue | null>(null);

export function DependencyGuardProvider({ children }: { children: ReactNode }) {
  const { toast } = useToast();
  const [pending, setPending] = useState<PendingAction | null>(null);
  const [busy, setBusy] = useState(false);

  const run = useCallback(async <T,>(action: (force: boolean) => Promise<T>, options: GuardOptions = {}) => {
    try {
      const result = await action(false);
      await options.onSuccess?.(result);
      if (options.successMessage) toast(options.successMessage, "success");
      return result;
    } catch (error) {
      const conflict = dependencyConflictOf(error);
      if (conflict) {
        setPending({ conflict, action, options });
        return undefined;
      }
      if (options.onError?.(error)) return undefined;
      toast(displayErrorMessage(error, options.fallback || "操作失败，请稍后重试"), "error");
      return undefined;
    }
  }, [toast]);

  const force = async () => {
    if (!pending || busy) return;
    setBusy(true);
    try {
      const result = await pending.action(true);
      const options = pending.options;
      setPending(null);
      await options.onSuccess?.(result);
      if (options.successMessage) toast(options.successMessage, "success");
    } catch (error) {
      const conflict = dependencyConflictOf(error);
      if (conflict) {
        setPending((current) => current ? { ...current, conflict } : current);
        toast("依赖关系已变化，请重新确认影响范围", "error");
        return;
      }
      if (pending.options.onError?.(error)) return;
      toast(displayErrorMessage(error, pending.options.fallback || "操作失败，请稍后重试"), "error");
    } finally {
      setBusy(false);
    }
  };

  const value = useMemo(() => ({ run }), [run]);
  const conflict = pending?.conflict;

  return (
    <DependencyGuardContext.Provider value={value}>
      {children}
      <Modal
        open={Boolean(conflict)}
        onClose={() => !busy && setPending(null)}
        title={conflict?.requires_force ? "该修改会重建对象" : "对象仍被引用"}
        subtitle={conflict?.requires_force
          ? "必须先确认影响范围；强制重建不会删除引用者"
          : "请先修改或删除引用者，系统不会静默级联删除"}
        size="lg"
        busy={busy}
        footer={
          <>
            <Button onClick={() => setPending(null)} disabled={busy}>取消</Button>
            {conflict?.requires_force ? (
              <Button variant="danger" loading={busy} onClick={() => void force()}>仍要重建（force=true）</Button>
            ) : (
              <Button variant="primary" onClick={() => setPending(null)}>知道了</Button>
            )}
          </>
        }
      >
        {conflict && (
          <div className="form-stack dependency-conflict">
            <div className="inline-warning">
              <AlertTriangle size={15} />
              {conflict.requires_force
                ? "这是身份字段（角色、绑定端口、聚合成员等）变更，等同重建对象。引用者会保留，但下一次应用可能受影响。"
                : "该对象仍被启用配置直接引用，删除会被拒绝。请按建议先修改引用者。"}
            </div>

            <div className="notice-block">
              <strong>{conflict.message || "配置依赖冲突"}</strong>
              <span>错误码：<code>{conflict.code}</code>{conflict.resource?.type ? ` · 对象：${typeLabel(conflict.resource.type)} ${conflict.resource.id || ""}` : ""}</span>
              {typeof conflict.graph_generation === "number" && <span>依赖图代号：{conflict.graph_generation}</span>}
            </div>

            <ReferenceTable title="谁引用了它（直接引用，会阻止修改）" items={conflict.referenced_by} tone="danger" />

            {conflict.impact.length > 0 && (
              <ReferenceTable title="传递影响（间接依赖）" items={conflict.impact} tone="warning" />
            )}

            {conflict.cascade.length > 0 && (
              <ReferenceTable title="强制重建后的影响清单" items={conflict.cascade} tone="warning" />
            )}

            {conflict.suggestion && (
              <div className="notice-block">
                <strong><Info size={13} /> 可执行建议</strong>
                <span>{conflict.suggestion}</span>
              </div>
            )}

            {!conflict.requires_force && (
              <p className="muted">
                删除操作不提供强制级联。请到对应页面修改引用字段，或先删除引用者，再回来重试。
              </p>
            )}
          </div>
        )}
      </Modal>
    </DependencyGuardContext.Provider>
  );
}

export function useDependencyGuard() {
  const context = useContext(DependencyGuardContext);
  if (!context) throw new Error("useDependencyGuard must be used inside DependencyGuardProvider");
  return context;
}
