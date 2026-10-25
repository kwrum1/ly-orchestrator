import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Cable, Link2, RefreshCw, Settings2, ShieldAlert, Trash2 } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { API } from "../api/endpoints";
import { deleteJson, patchJson, postJson } from "../api/client";
import { policyAllowsFallback, setAfFallbackConsent, useDataplanePolicy } from "../api/dataplane";
import { useInterfaceBonds, useInterfacesResource, useInterfacesTelemetry } from "../api/hooks";
import type { InterfaceBond, InterfaceItem } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, Select, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { formatRate, roleLabel, text } from "../lib/format";
import { humanErrorMessage, linkInfo, takeoverInfo, takeoverSummary, type TakeoverInfo } from "../lib/human";

function interfaceRole(item: InterfaceItem) {
  return item.gateway_role || item.role || item.mode_role?.gateway || "";
}

function isManagement(item: InterfaceItem) {
  return Boolean(item.management) || interfaceRole(item) === "management" || String(item.name || "").toLowerCase() === "eth0";
}

const LOGICAL_KINDS = new Set(["lan_bridge", "bridge", "vlan", "wan", "proxy", "egress", "subscription", "logical", "bond", "bond_member"]);
const LOGICAL_ID = /^(lan-|lan_|lan-bridge-|br-|wan-|wan_|proxy-|egress-|bond-)/i;

/**
 * The NIC page only owns physical ports. Logical LAN/WAN objects (bridges,
 * display-name overrides, proxy egresses) are managed by their own pages.
 */
function isLogicalInterface(item: any) {
  const kind = String(item?.kind || item?.type || "").toLowerCase();
  if (LOGICAL_KINDS.has(kind)) return true;
  const id = String(item?.id || "");
  const name = String(item?.name || "");
  if (LOGICAL_ID.test(id) || LOGICAL_ID.test(name)) return true;
  if (item?.display_name && !item?.system_name && !item?.mac && !item?.speed) return true;
  return false;
}

function portKey(item: any) {
  return String(item?.system_name || item?.id || item?.name || "");
}

function memberKeys(bond: InterfaceBond) {
  return (bond.members || []).map((member) => String(member));
}

function itemMatchesMembers(item: any, members: string[]) {
  const keys = [item?.id, item?.name, item?.system_name].filter(Boolean).map(String);
  return keys.some((key) => members.includes(key));
}

function isBondRow(item: any) {
  return Boolean(item?.__bond);
}

interface PortRow extends InterfaceItem {
  __bond?: boolean;
  __takeover?: TakeoverInfo;
  members?: string[];
  memberCount?: number;
}

function bondTakeover(members: InterfaceItem[]): TakeoverInfo {
  if (!members.length) {
    return { kind: "available", label: "可接管（高性能）", tone: "info", assignable: true, state: "available", detail: "聚合组成员信息暂不可用。" };
  }
  const states = members.map((member) => takeoverInfo(member));
  if (states.some((state) => state.kind === "compat")) {
    return { kind: "compat", label: "已接管 · 兼容模式（性能受限）", tone: "warning", assignable: true, state: "taken_over", detail: "聚合组中至少一张成员网卡使用兼容模式，整体吞吐会受限。" };
  }
  if (states.every((state) => state.kind === "native")) {
    return { kind: "native", label: "高性能（原生）", tone: "success", assignable: true, state: "taken_over", detail: "聚合组的成员网卡均使用高性能接管。" };
  }
  if (states.some((state) => state.kind === "unsupported")) {
    return { kind: "unsupported", label: "不支持接管", tone: "neutral", assignable: false, state: "unsupported", detail: "聚合组中有成员网卡不支持接管。" };
  }
  return { kind: "available", label: "可接管（高性能）", tone: "info", assignable: true, state: "available", detail: "聚合组等待成员网卡完成接管。" };
}

function mergeTakeover(item: any, fallback: TakeoverInfo) {
  return item?.__takeover || fallback;
}

function displayTakeover(item: any): TakeoverInfo {
  if (!item?.__bond && isManagement(item)) {
    return { kind: "management", label: "管理口", tone: "neutral", assignable: false, state: "management", detail: "管理口由系统保留，不参与数据面接管。" };
  }
  return takeoverInfo(item);
}

export function InterfacesPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { requestAfConsent } = apply;
  const { session } = useAuth();
  const resource = useInterfacesResource();
  const telemetry = useInterfacesTelemetry();
  const bondsQuery = useInterfaceBonds();
  const policy = useDataplanePolicy();
  const [roleTarget, setRoleTarget] = useState<InterfaceItem | null>(null);
  const [roleValue, setRoleValue] = useState("wan");
  const [bondOpen, setBondOpen] = useState(false);
  const [bondName, setBondName] = useState("");
  const [bondMembers, setBondMembers] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [policyBusy, setPolicyBusy] = useState(false);

  const telemetryItems = Array.isArray(telemetry.data?.data) ? telemetry.data?.data as InterfaceItem[] : [];
  const resourceItems = resource.data?.items || [];
  const merged = useMemo(() => {
    const source = resourceItems.length ? resourceItems : telemetryItems;
    return source.map((item) => {
      const live = telemetryItems.find((candidate) =>
        (item.id && candidate.id === item.id) ||
        (item.name && candidate.name === item.name) ||
        (item.system_name && candidate.system_name === item.system_name)
      );
      return { ...item, ...live } as InterfaceItem;
    });
  }, [resourceItems, telemetryItems]);

  const bonds = (bondsQuery.data?.items || []) as InterfaceBond[];
  const allMembers = useMemo(() => bonds.flatMap(memberKeys), [bonds]);

  const physicalPorts = useMemo(
    () => merged.filter((item) => !isLogicalInterface(item) && !isBondRow(item)),
    [merged]
  );

  const bondRows = useMemo<PortRow[]>(() => bonds.map((bond) => {
    const members = physicalPorts.filter((item) => itemMatchesMembers(item, memberKeys(bond)));
    const up = members.length > 0 && members.every((item) => item.link_state === "up");
    const pending = members.some((item) => linkInfo(item).pending);
    return {
      ...bond,
      __bond: true,
      __takeover: bondTakeover(members),
      id: bond.id,
      name: bond.name || bond.id,
      members: bond.members || [],
      memberCount: members.length,
      link_state: bond.link_state || (up ? "up" : "down"),
      link_speed_mbps: bond.link_speed_mbps || members[0]?.link_speed_mbps,
      verification: bond.verification || (pending ? "pending_link" : undefined),
      speed: bond.speed || members[0]?.speed,
      rx_bps: members.reduce((sum, item) => sum + Number(item.rx_bps || item.stats?.rx_bps || 0), 0),
      tx_bps: members.reduce((sum, item) => sum + Number(item.tx_bps || item.stats?.tx_bps || 0), 0),
      gateway_role: bond.gateway_role || "",
    } as PortRow;
  }), [bonds, physicalPorts]);

  // Bond member ports are no longer independent NICs.
  const visiblePorts = useMemo(
    () => physicalPorts.filter((item) => !itemMatchesMembers(item, allMembers)),
    [physicalPorts, allMembers]
  );
  const visibleItems = useMemo<PortRow[]>(() => [...visiblePorts, ...bondRows], [visiblePorts, bondRows]);
  const assignablePorts = useMemo(() => physicalPorts.filter((item) => !isManagement(item)), [physicalPorts]);
  const summary = useMemo(() => takeoverSummary(assignablePorts), [assignablePorts]);
  const compatAllowed = policyAllowsFallback(policy.data);

  const candidates = useMemo(
    () => physicalPorts.filter((item) =>
      !isManagement(item) &&
      !itemMatchesMembers(item, allMembers) &&
      takeoverInfo(item).assignable
    ),
    [physicalPorts, allMembers]
  );

  const columns = useMemo<ColumnDef<PortRow, any>[]>(() => [
    {
      id: "name",
      header: "接口",
      accessorFn: (row) => row.name || row.id,
      cell: ({ row }) => {
        const item = row.original;
        return (
          <span className="interface-name">
            {isBondRow(item) ? <Link2 size={15} /> : <Cable size={15} />}
            <strong>{text(item.name || item.id)}</strong>
            {isBondRow(item) ? (
              <small>聚合组 · 成员 {item.members?.length ? item.members.join("、") : "--"}</small>
            ) : (
              item.system_name && <small>{item.system_name}</small>
            )}
          </span>
        );
      },
      meta: { width: 220 },
    },
    {
      id: "takeover",
      header: "接管能力",
      accessorFn: (row) => mergeTakeover(row, displayTakeover(row)).label,
      cell: ({ row }) => {
        const info = mergeTakeover(row.original, displayTakeover(row.original));
        return <Badge tone={info.tone} title={info.detail}>{info.label}</Badge>;
      },
      meta: { width: 190 },
    },
    {
      id: "state",
      header: "链路",
      accessorFn: (row) => linkInfo(row).label,
      cell: ({ row }) => {
        const link = linkInfo(row.original);
        return <span className={`link-chip${link.down ? " is-down" : ""}${link.pending ? " is-pending" : ""}`}>{link.label}</span>;
      },
      meta: { width: 110 },
    },
    {
      id: "role",
      header: "方向",
      accessorFn: (row) => interfaceRole(row),
      cell: ({ row }) => isBondRow(row.original)
        ? <Badge tone="violet">聚合组</Badge>
        : <Badge tone={isManagement(row.original) ? "neutral" : interfaceRole(row.original) === "wan" ? "info" : "success"}>{roleLabel(interfaceRole(row.original)) || "未配置"}</Badge>,
      meta: { width: 95 },
    },
    { id: "rx", header: "接收", accessorFn: (row) => row.rx_bps ?? row.stats?.rx_bps, cell: ({ getValue }) => formatRate(getValue()), meta: { width: 105, align: "right" } },
    { id: "tx", header: "发送", accessorFn: (row) => row.tx_bps ?? row.stats?.tx_bps, cell: ({ getValue }) => formatRate(getValue()), meta: { width: 105, align: "right" } },
    { id: "mac", header: "MAC", accessorFn: (row) => row.mac, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    {
      id: "actions",
      header: "操作",
      cell: ({ row }) => {
        if (isBondRow(row.original)) {
          return (
            <Button
              size="sm"
              variant="text"
              icon={<Trash2 size={14} />}
              disabled={session?.role === "readonly"}
              onClick={() => void removeBond(row.original)}
            >
              删除聚合
            </Button>
          );
        }
        const info = displayTakeover(row.original);
        const portName = String(row.original.name || row.original.id || "");
        if (info.kind === "consent") {
          return (
            <Button
              size="sm"
              variant="text"
              icon={<ShieldAlert size={14} />}
              disabled={session?.role === "readonly"}
              onClick={() => requestAfConsent([portName])}
            >
              启用兼容模式
            </Button>
          );
        }
        return (
          <Button
            size="sm"
            variant="text"
            icon={<Settings2 size={14} />}
            disabled={!info.assignable || session?.role === "readonly"}
            title={!info.assignable ? "该网卡暂不支持接管，不能分配角色" : undefined}
            onClick={() => { setRoleTarget(row.original); setRoleValue(interfaceRole(row.original) === "lan" ? "lan" : "wan"); }}
          >
            角色配置
          </Button>
        );
      },
      meta: { width: 150, align: "right", hideable: false },
    },
  ], [session?.role, bonds]);

  const refresh = () => {
    resource.refetch();
    telemetry.refetch();
    bondsQuery.refetch();
    policy.refetch();
  };

  const saveRole = async () => {
    if (!roleTarget) return;
    const ok = await confirm({
      title: `将 ${roleTarget.name || roleTarget.id} 配置为 ${roleValue.toUpperCase()}？`,
      message: "角色变更会影响依赖该接口的 LAN/WAN、策略与 DHCP 配置。",
      detail: <span>保存后会立即应用；如依赖配置不完整，应用会说明原因。</span>,
      confirmLabel: "保存并应用",
    });
    if (!ok) return;
    setBusy(true);
    try {
      await patchJson(API.resources.interface(roleTarget.id || roleTarget.name || ""), {
        id: roleTarget.id || roleTarget.name,
        name: roleTarget.name || roleTarget.id,
        gateway_role: roleValue,
        mode_role: { gateway: roleValue, bridge: null },
        candidate_scopes: [roleValue],
        role_configured: true,
        runtime_state: "role_configured",
      });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
      await queryClient.invalidateQueries({ queryKey: ["telemetry", "interfaces"] });
      toast("接口角色已保存，正在应用", "success");
      setRoleTarget(null);
      void apply.apply();
    } catch (error: any) {
      toast(humanErrorMessage(error, "保存接口角色失败"), "error");
    } finally {
      setBusy(false);
    }
  };

  const saveBond = async () => {
    if (!bondName.trim()) {
      toast("请填写聚合组名称", "error");
      return;
    }
    if (bondMembers.length < 2) {
      toast("请选择至少两张网卡", "error");
      return;
    }
    setBusy(true);
    try {
      await postJson(API.resources.interfaceBonds, {
        id: `bond-${crypto.randomUUID?.() || Date.now()}`,
        name: bondName.trim(),
        members: bondMembers,
      });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interface-bonds"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
      await queryClient.invalidateQueries({ queryKey: ["telemetry", "interfaces"] });
      toast("聚合组已创建", "success");
      setBondOpen(false);
      setBondName("");
      setBondMembers([]);
      void apply.apply();
    } catch (error: any) {
      toast(humanErrorMessage(error, "创建聚合失败"), "error");
    } finally {
      setBusy(false);
    }
  };

  async function removeBond(bond: PortRow) {
    const ok = await confirm({
      title: `删除聚合组 ${bond.name || bond.id}？`,
      message: "删除后，成员网卡会恢复为独立端口，引用该聚合组的线路或策略需要重新配置。",
      confirmLabel: "删除",
      danger: true,
    });
    if (!ok) return;
    try {
      await deleteJson(API.resources.interfaceBond(bond.id));
      await queryClient.invalidateQueries({ queryKey: ["resources", "interface-bonds"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
      await queryClient.invalidateQueries({ queryKey: ["telemetry", "interfaces"] });
      toast("聚合组已删除，成员网卡已恢复", "success");
      void apply.apply();
    } catch (error: any) {
      toast(humanErrorMessage(error, "删除聚合组失败"), "error");
    }
  }

  const changeCompat = async (enabled: boolean) => {
    if (enabled) {
      const ok = await confirm({
        title: "允许使用兼容模式？",
        message: "无法启用高性能接管的网卡可以改用兼容模式。兼容模式会降低吞吐并提高 CPU 占用，仅建议在必要时启用。",
        confirmLabel: "允许兼容模式",
        danger: true,
      });
      if (!ok) return;
    }
    setPolicyBusy(true);
    try {
      await setAfFallbackConsent(enabled);
      await queryClient.invalidateQueries({ queryKey: ["dataplane", "policy"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
      await queryClient.invalidateQueries({ queryKey: ["telemetry", "interfaces"] });
      toast(enabled ? "已允许兼容模式，正在重新应用" : "已关闭兼容模式，将优先使用高性能接管", "success");
      void apply.apply();
    } catch (error: any) {
      toast(humanErrorMessage(error, enabled ? "启用兼容模式失败" : "关闭兼容模式失败"), "error");
    } finally {
      setPolicyBusy(false);
    }
  };

  return (
    <div className="page">
      <PageHeader
        title="网卡设置"
        subtitle="仅显示物理网卡与聚合组；LAN/WAN 等逻辑接口请到对应页面管理"
        actions={
          <>
            <Button size="sm" icon={<RefreshCw size={14} />} onClick={refresh}>刷新</Button>
            <Button size="sm" variant="primary" icon={<Link2 size={14} />} disabled={session?.role === "readonly" || candidates.length < 2} onClick={() => setBondOpen(true)}>创建聚合</Button>
          </>
        }
      />

      <Card
        title="网卡接管"
        subtitle="优先使用高性能方式接管；兼容模式只在确认后启用"
        actions={
          <Badge tone={summary.mixed ? "warning" : summary.native + summary.compat > 0 ? "success" : "info"} dot>
            {summary.mixed ? "部分兼容模式 · 性能受限" : summary.native + summary.compat > 0 ? "已接管" : "可接管"}
          </Badge>
        }
      >
        <div className="takeover-panel">
          <div className="takeover-facts">
            <span><i className="status-dot status-dot-success" />高性能 {summary.native} 张</span>
            <span><i className="status-dot status-dot-warning" />兼容模式 {summary.compat} 张</span>
            <span><i className="status-dot status-dot-info" />可接管 {summary.available + summary.consent} 张</span>
            <span><i className="status-dot status-dot-neutral" />不支持 {summary.unsupported} 张</span>
          </div>
          <p className="muted">
            {summary.compat > 0
              ? "部分网卡正在使用兼容模式，吞吐和时延会受到影响。"
              : summary.available + summary.consent > 0
                ? "部分网卡尚未完成接管；网线未连接不影响接管能力，插线后会自动完成。"
                : "设备会优先尝试高性能接管；无法接管的网卡不会分配线路，也不影响其他网卡。"}
          </p>
          <Button
            size="sm"
            variant={compatAllowed ? "secondary" : "ghost"}
            icon={<ShieldAlert size={14} />}
            loading={policyBusy}
            disabled={session?.role === "readonly"}
            onClick={() => void changeCompat(!compatAllowed)}
          >
            {compatAllowed ? "关闭兼容模式" : "允许兼容模式（性能受限）"}
          </Button>
        </div>
      </Card>

      <Card title="接口总览" subtitle={`${visibleItems.length} 个接口 · ${bonds.length} 个聚合组`}>
        <div className="port-grid port-grid-large">
          {visibleItems.map((item) => {
            const info = mergeTakeover(item, displayTakeover(item));
            const link = linkInfo(item);
            return (
              <div className="port-card static" key={item.id || item.name}>
                <span className="port-icon">{isBondRow(item) ? <Link2 size={16} /> : <Cable size={16} />}</span>
                <span className="port-name">
                  {item.name || item.id}
                  <small>{isBondRow(item) ? `聚合 · ${item.members?.length || 0} 张成员` : interfaceRole(item) ? roleLabel(interfaceRole(item)) : "未配置"}</small>
                </span>
                <span className="port-takeover" title={info.detail}><Badge tone={info.tone}>{info.label}</Badge></span>
                <span className={`link-chip${link.down ? " is-down" : ""}${link.pending ? " is-pending" : ""}`}>{link.label}</span>
                <span className="port-rate"><span>↓ {formatRate(item.rx_bps ?? item.stats?.rx_bps)}</span><span>↑ {formatRate(item.tx_bps ?? item.stats?.tx_bps)}</span></span>
              </div>
            );
          })}
        </div>
        {!visibleItems.length && <p className="muted">暂无物理网卡。</p>}
      </Card>

      <DataTable
        data={visibleItems}
        columns={columns}
        loading={resource.isLoading && telemetry.isLoading}
        error={resource.isError && !visibleItems.length ? resource.error : null}
        onRetry={refresh}
        emptyTitle="暂无接口"
        emptyDescription="未发现物理网卡。"
        getRowId={(row, index) => row.id || `${row.name}-${index}`}
      />

      <Modal
        open={Boolean(roleTarget)}
        onClose={() => !busy && setRoleTarget(null)}
        title="角色配置"
        subtitle={roleTarget ? `${roleTarget.name || roleTarget.id} · ${displayTakeover(roleTarget).label}` : ""}
        size="sm"
        busy={busy}
        footer={<><Button onClick={() => setRoleTarget(null)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={saveRole}>保存并应用</Button></>}
      >
        <div className="form-stack">
          <div className="notice-block"><strong>当前接口</strong><span>{roleTarget?.name || roleTarget?.id} · {roleTarget?.mac || "无 MAC"}</span></div>
          <Field label="方向" hint="管理口保留在管理面，不能配置为 WAN 或 LAN">
            <Select value={roleValue} onChange={(event) => setRoleValue(event.target.value)}>
              <option value="wan">WAN 接口</option>
              <option value="lan">LAN 接口</option>
            </Select>
          </Field>
          <div className="inline-warning">保存后会立即应用；如依赖配置不完整，应用会说明原因。</div>
        </div>
      </Modal>

      <Modal
        open={bondOpen}
        onClose={() => !busy && setBondOpen(false)}
        title="创建聚合"
        subtitle="选择至少两张速率相同的已接管网卡"
        size="md"
        busy={busy}
        footer={<><Button onClick={() => setBondOpen(false)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={saveBond}>创建</Button></>}
      >
        <div className="form-stack">
          <Field label="聚合组名称" required><Input value={bondName} onChange={(event) => setBondName(event.target.value)} placeholder="例如：核心上联" /></Field>
          <div className="check-grid">
            {candidates.map((item) => {
              const info = takeoverInfo(item);
              return (
                <label key={item.id} className="check-card">
                  <input
                    type="checkbox"
                    checked={bondMembers.includes(item.id)}
                    onChange={(event) => setBondMembers((current) => event.target.checked ? [...current, item.id] : current.filter((id) => id !== item.id))}
                  />
                  <span>
                    <strong>{item.name || item.id}</strong>
                    <small>{item.speed || "--"} · {info.label}</small>
                  </span>
                </label>
              );
            })}
          </div>
          {!candidates.length && <div className="inline-warning">没有可加入聚合组的网卡；不支持的网卡不会出现在这里。</div>}
        </div>
      </Modal>
    </div>
  );
}
