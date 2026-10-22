import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Cable, Link2, RefreshCw, Settings2 } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { API } from "../api/endpoints";
import { patchJson, postJson } from "../api/client";
import { useInterfacesResource, useInterfacesTelemetry } from "../api/hooks";
import type { InterfaceItem } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, Select, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { formatRate, roleLabel, stateLabel, text } from "../lib/format";

function interfaceRole(item: InterfaceItem) {
  return item.gateway_role || item.role || item.mode_role?.gateway || "";
}

function isManagement(item: InterfaceItem) {
  return interfaceRole(item) === "management" || String(item.name || "").toLowerCase() === "eth0";
}

export function InterfacesPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const resource = useInterfacesResource();
  const telemetry = useInterfacesTelemetry();
  const [roleTarget, setRoleTarget] = useState<InterfaceItem | null>(null);
  const [roleValue, setRoleValue] = useState("wan");
  const [bondOpen, setBondOpen] = useState(false);
  const [bondName, setBondName] = useState("");
  const [bondMembers, setBondMembers] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);

  const telemetryItems = Array.isArray(telemetry.data?.data) ? telemetry.data?.data as InterfaceItem[] : [];
  const items = useMemo(() => {
    const source = resource.data?.items || [];
    if (!source.length) return telemetryItems;
    return source.map((item) => {
      const live = telemetryItems.find((candidate) => candidate.id === item.id || candidate.name === item.name || candidate.system_name === item.system_name);
      return { ...item, ...live };
    });
  }, [resource.data, telemetryItems]);

  const members = items.filter((item) => !isManagement(item) && item.link_state !== "down");

  const columns = useMemo<ColumnDef<InterfaceItem, any>[]>(() => [
    { id: "name", header: "接口", accessorFn: (row) => row.name || row.id, cell: ({ row }) => <span className="interface-name"><Cable size={15} /><strong>{text(row.original.name || row.original.id)}</strong>{row.original.system_name && <small>{row.original.system_name}</small>}</span>, meta: { width: 180 } },
    { id: "state", header: "状态", accessorFn: (row) => row.link_state, cell: ({ row }) => <Badge tone={row.original.link_state === "up" ? "success" : "danger"} dot>{row.original.link_state === "up" ? "LINKUP" : text(row.original.link_state, "DOWN").toUpperCase()}</Badge>, meta: { width: 100 } },
    { id: "mode", header: "工作模式", accessorFn: (row) => row.work_mode, cell: ({ getValue }) => text(getValue()), meta: { width: 130 } },
    { id: "role", header: "方向", accessorFn: (row) => interfaceRole(row), cell: ({ row }) => <Badge tone={isManagement(row.original) ? "neutral" : interfaceRole(row.original) === "wan" ? "info" : "success"}>{roleLabel(interfaceRole(row.original)) || "未配置"}</Badge>, meta: { width: 95 } },
    { id: "speed", header: "速率", accessorFn: (row) => row.speed, cell: ({ getValue }) => text(getValue()), meta: { width: 90 } },
    { id: "rx", header: "接收", accessorFn: (row) => row.rx_bps ?? row.stats?.rx_bps, cell: ({ getValue }) => formatRate(getValue()), meta: { width: 105, align: "right" } },
    { id: "tx", header: "发送", accessorFn: (row) => row.tx_bps ?? row.stats?.tx_bps, cell: ({ getValue }) => formatRate(getValue()), meta: { width: 105, align: "right" } },
    { id: "mac", header: "MAC", accessorFn: (row) => row.mac, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150 } },
    {
      id: "actions",
      header: "操作",
      cell: ({ row }) => (
        <Button
          size="sm"
          variant="text"
          icon={<Settings2 size={14} />}
          disabled={isManagement(row.original) || session?.role === "readonly"}
          onClick={() => { setRoleTarget(row.original); setRoleValue(interfaceRole(row.original) === "lan" ? "lan" : "wan"); }}
        >
          角色配置
        </Button>
      ),
      meta: { width: 100, align: "right", hideable: false },
    },
  ], [session?.role]);

  const saveRole = async () => {
    if (!roleTarget) return;
    const ok = await confirm({
      title: `将 ${roleTarget.name || roleTarget.id} 配置为 ${roleValue.toUpperCase()}？`,
      message: "角色变更会影响依赖该接口的 LAN/WAN、策略与 DHCP 配置。",
      detail: <span>保存后会立即触发运行态应用；失败时后端会保留期望状态并返回原因。</span>,
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
      toast(error?.message || "保存接口角色失败", "error", error?.requestId);
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
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
      toast("聚合组已创建", "success");
      setBondOpen(false);
      setBondName("");
      setBondMembers([]);
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "创建聚合失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="page">
      <PageHeader
        title="网卡设置"
        subtitle="接口角色、链路状态与实时速率；管理口不可改为 WAN/LAN"
        actions={
          <>
            <Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { resource.refetch(); telemetry.refetch(); }}>刷新</Button>
            <Button size="sm" variant="primary" icon={<Link2 size={14} />} disabled={session?.role === "readonly"} onClick={() => setBondOpen(true)}>创建聚合</Button>
          </>
        }
      />

      <Card title="接口总览" subtitle={`${items.length} 个接口`}>
        <div className="port-grid port-grid-large">
          {items.map((item) => (
            <div className="port-card static" key={item.id || item.name}>
              <span className="port-icon"><Cable size={16} /></span>
              <span className="port-name">{item.name || item.id}<small>{interfaceRole(item) ? roleLabel(interfaceRole(item)) : item.work_mode || "未配置"}</small></span>
              <span className="port-state"><i className={`status-dot status-dot-${item.link_state === "up" ? "success" : "danger"}`} />{item.link_state === "up" ? "LINKUP" : "DOWN"}</span>
              <span className="port-rate"><span>↓ {formatRate(item.rx_bps ?? item.stats?.rx_bps)}</span><span>↑ {formatRate(item.tx_bps ?? item.stats?.tx_bps)}</span></span>
            </div>
          ))}
        </div>
      </Card>

      <DataTable
        data={items}
        columns={columns}
        loading={resource.isLoading && telemetry.isLoading}
        error={resource.isError && !items.length ? resource.error : null}
        onRetry={() => { resource.refetch(); telemetry.refetch(); }}
        emptyTitle="暂无接口"
        emptyDescription="控制面未返回物理接口。"
        getRowId={(row, index) => row.id || `${row.name}-${index}`}
      />

      <Modal
        open={Boolean(roleTarget)}
        onClose={() => !busy && setRoleTarget(null)}
        title="角色配置"
        subtitle={roleTarget ? `${roleTarget.name || roleTarget.id} · ${roleTarget.work_mode || ""}` : ""}
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
          <div className="inline-warning">保存后会立即应用；如依赖配置不完整，应用会返回降级/失败原因。</div>
        </div>
      </Modal>

      <Modal
        open={bondOpen}
        onClose={() => !busy && setBondOpen(false)}
        title="创建聚合"
        subtitle="选择至少两张同速率、同工作模式的网卡"
        size="md"
        busy={busy}
        footer={<><Button onClick={() => setBondOpen(false)} disabled={busy}>取消</Button><Button variant="primary" loading={busy} onClick={saveBond}>创建</Button></>}
      >
        <div className="form-stack">
          <Field label="聚合组名称" required><Input value={bondName} onChange={(event) => setBondName(event.target.value)} placeholder="例如：核心上联" /></Field>
          <div className="check-grid">
            {members.map((item) => (
              <label key={item.id} className="check-card">
                <input
                  type="checkbox"
                  checked={bondMembers.includes(item.id)}
                  onChange={(event) => setBondMembers((current) => event.target.checked ? [...current, item.id] : current.filter((id) => id !== item.id))}
                />
                <span><strong>{item.name || item.id}</strong><small>{item.speed || "--"} · {item.work_mode || "--"}</small></span>
              </label>
            ))}
          </div>
        </div>
      </Modal>
    </div>
  );
}
