import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Cable, Network, Plus, RefreshCw, Save, Trash2, Waypoints } from "lucide-react";
import { API } from "../../api/endpoints";
import { putJson } from "../../api/client";
import { useInterfacesResource } from "../../api/hooks";
import { useOrchestratorTopology } from "../../api/orchestrator-hooks";
import type { OrchestrationGroup, Topology, TopologyInterface } from "../../api/types";
import { Badge, Button, Card, Field, Input, PageHeader, Select, Skeleton, useToast } from "../../components/ui";
import { useAuth, useConfirm } from "../../state/providers";
import { roleLabel, text } from "../../lib/format";

function emptyTopology(management: string): Topology {
  return {
    schema_version: 1,
    management_interface: management,
    interfaces: [
      { name: "lan", role: "lan", port: "" },
      { name: "wan", role: "wan", port: "" },
    ],
    orchestration_groups: [],
  };
}

function interfaceFor(topology: Topology, role: "lan" | "wan") {
  return topology.interfaces.find((item) => item.role === role);
}

function interfacePort(item?: TopologyInterface) {
  return item?.port || (item?.bond?.members || []).join(", ") || "";
}

export function OrchestratorTopologyPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const topologyQuery = useOrchestratorTopology();
  const inventory = useInterfacesResource();
  const [draft, setDraft] = useState<Topology | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const inventoryItems = inventory.data?.items || [];
  const managementDefault = useMemo(() => {
    const management = inventoryItems.find((item) => item.gateway_role === "management" || item.mode_role?.gateway === "management" || item.name === "eth0");
    return management?.name || inventoryItems[0]?.name || "";
  }, [inventoryItems]);

  useEffect(() => {
    if (topologyQuery.data?.item) setDraft(topologyQuery.data.item);
    else if (topologyQuery.data === null) setDraft(emptyTopology(managementDefault));
  }, [topologyQuery.data, managementDefault]);

  const physicalPorts = inventoryItems.filter((item) => (item.name || item.id) !== draft?.management_interface);
  const usedPorts = useMemo(() => {
    const used = new Set<string>();
    if (draft?.management_interface) used.add(draft.management_interface);
    for (const item of draft?.interfaces || []) {
      if (item.port) used.add(item.port);
      for (const member of item.bond?.members || []) used.add(member);
    }
    for (const group of draft?.orchestration_groups || []) {
      for (const port of group.ports) {
        if (port.interface) used.add(port.interface);
        for (const member of port.bond?.members || []) used.add(member);
      }
    }
    return used;
  }, [draft]);

  const setInterfacePort = (role: "lan" | "wan", port: string) => {
    if (!draft) return;
    const interfaces = draft.interfaces.map((item) => item.role === role ? { ...item, port, bond: null } : item);
    setDraft({ ...draft, interfaces });
  };

  const setInterfaceBond = (role: "lan" | "wan", members: string[]) => {
    if (!draft) return;
    const interfaces = draft.interfaces.map((item) => item.role === role ? {
      ...item,
      port: "",
      bond: members.length ? { name: `bond-${role}`, members } : null,
    } : item);
    setDraft({ ...draft, interfaces });
  };

  const addGroup = () => {
    if (!draft) return;
    setDraft({
      ...draft,
      orchestration_groups: [...draft.orchestration_groups, {
        name: `group-${draft.orchestration_groups.length + 1}`,
        ports: [{ interface: "", direction: "lan_facing" }, { interface: "", direction: "wan_facing" }],
      }],
    });
  };

  const updateGroup = (index: number, patch: Partial<OrchestrationGroup>) => {
    if (!draft) return;
    const groups = draft.orchestration_groups.map((group, position) => position === index ? { ...group, ...patch } : group);
    setDraft({ ...draft, orchestration_groups: groups });
  };

  const updateGroupPort = (groupIndex: number, direction: "lan_facing" | "wan_facing", port: string) => {
    if (!draft) return;
    const groups = draft.orchestration_groups.map((group, index) => index === groupIndex ? {
      ...group,
      ports: group.ports.map((item) => item.direction === direction ? { ...item, interface: port } : item),
    } : group);
    setDraft({ ...draft, orchestration_groups: groups });
  };

  const removeGroup = async (index: number) => {
    if (!draft) return;
    const group = draft.orchestration_groups[index];
    if (!(await confirm({ title: `移除编排组 ${group.name}？`, message: "保存拓扑后会立即触发透明编排；失败时后端会回滚。", confirmLabel: "移除", danger: true }))) return;
    setDraft({ ...draft, orchestration_groups: draft.orchestration_groups.filter((_, position) => position !== index) });
  };

  const save = async () => {
    if (!draft) return;
    if (!interfaceFor(draft, "lan")?.port && !interfaceFor(draft, "lan")?.bond?.members.length) { toast("请配置逻辑 LAN 接口", "error"); return; }
    if (!interfaceFor(draft, "wan")?.port && !interfaceFor(draft, "wan")?.bond?.members.length) { toast("请配置逻辑 WAN 接口", "error"); return; }
    for (const group of draft.orchestration_groups) {
      if (!group.name.trim()) { toast("编排组名称不能为空", "error"); return; }
      const lan = group.ports.find((item) => item.direction === "lan_facing")?.interface;
      const wan = group.ports.find((item) => item.direction === "wan_facing")?.interface;
      if (!lan || !wan) { toast(`编排组 ${group.name} 需要 LAN 侧和 WAN 侧端口`, "error"); return; }
    }
    setBusy(true);
    setError("");
    try {
      const payload = {
        schema_version: 1,
        management_interface: draft.management_interface,
        interfaces: draft.interfaces.map((item) => item.bond?.members.length
          ? { name: item.name, role: item.role, bond: item.bond }
          : { name: item.name, role: item.role, port: item.port }),
        orchestration_groups: draft.orchestration_groups.map((group) => ({
          name: group.name.trim(),
          ports: group.ports.map((port) => ({ interface: port.interface, direction: port.direction })),
        })),
      };
      const result = await putJson<any>(API.orchestrator.topology, payload, 30_000);
      toast("拓扑已保存", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
    } catch (failed: any) {
      setError(failed?.message || "保存拓扑失败");
      toast(failed?.message || "保存拓扑失败", "error");
    } finally {
      setBusy(false);
    }
  };

  if (topologyQuery.isLoading || !draft) return <div className="page"><Skeleton rows={8} /></div>;
  if (topologyQuery.isError) return <div className="page"><Card title="拓扑读取失败"><div className="error-state">{String((topologyQuery.error as any)?.message || topologyQuery.error)}</div></Card></div>;

  return (
    <div className="page">
      <PageHeader
        title="单机拓扑"
        subtitle="本地管理口、逻辑 LAN/WAN 与编排组；当前产品不是多站点/跨网关纳管"
        actions={<><Button size="sm" icon={<RefreshCw size={14} />} onClick={() => topologyQuery.refetch()}>刷新</Button><Button size="sm" variant="primary" icon={<Save size={14} />} loading={busy} onClick={save} disabled={session?.role === "readonly"}>保存拓扑</Button></>}
        meta={<><span className="meta-item">本机唯一拓扑</span><span className="meta-item">checksum {text(topologyQuery.data?.checksum, "未保存")}</span></>}
      />

      {!topologyQuery.data && <div className="inline-warning">当前还没有已保存的编排拓扑。填写 LAN/WAN 与可选编排组后保存；首次保存会触发透明编排。</div>}
      {error && <div className="error-state"><strong>保存失败</strong><p>{error}</p></div>}

      <div className="orch-topology-grid">
        <Card title="逻辑接口" subtitle="LAN/WAN 各一个；端口不能与编排组/管理口重复">
          <div className="form-grid">
            <Field label="管理接口" hint="管理口由 Linux 管理，不能加入 LAN/WAN 或编排组">
              <Input value={draft.management_interface} onChange={(event) => setDraft({ ...draft, management_interface: event.target.value })} placeholder="eth0" />
            </Field>
            <div />
            {(["lan", "wan"] as const).map((role) => {
              const item = interfaceFor(draft, role);
              const bond = item?.bond?.members || [];
              return (
                <div className="span-2 role-editor" key={role}>
                  <div className="role-editor-head"><Badge tone={role === "lan" ? "success" : "info"}>{role.toUpperCase()}</Badge><strong>{item ? (interfacePort(item) || "未配置") : "未配置"}</strong></div>
                  <div className="form-grid">
                    <Field label="类型"><Select value={bond.length ? "bond" : "port"} onChange={(event) => event.target.value === "bond" ? setInterfaceBond(role, [physicalPorts[0]?.name || ""]) : setInterfacePort(role, physicalPorts[0]?.name || "")}><option value="port">单端口</option><option value="bond">链路聚合</option></Select></Field>
                    {!bond.length ? (
                      <Field label="端口">
                        <Select value={item?.port || ""} onChange={(event) => setInterfacePort(role, event.target.value)}>
                          <option value="">请选择端口</option>
                          {physicalPorts.map((port) => <option key={port.id} value={port.name || port.id}>{port.name || port.id} · {port.speed || "--"}</option>)}
                        </Select>
                      </Field>
                    ) : (
                      <Field label="聚合成员">
                        <div className="check-grid">
                          {physicalPorts.map((port) => (
                            <label className="check-card" key={port.id}>
                              <input type="checkbox" checked={bond.includes(port.name || port.id)} onChange={(event) => setInterfaceBond(role, event.target.checked ? [...bond, port.name || port.id] : bond.filter((member) => member !== (port.name || port.id)))} />
                              <span><strong>{port.name || port.id}</strong><small>{port.speed || "--"}</small></span>
                            </label>
                          ))}
                        </div>
                      </Field>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        </Card>

        <Card title="物理端口使用" subtitle="来自 /interfaces 与当前拓扑草稿">
          <div className="orch-port-list">
            {inventoryItems.map((item) => {
              const name = item.name || item.id;
              const used = usedPorts.has(name);
              const role = name === draft.management_interface ? "管理口" : interfaceFor(draft, "lan")?.port === name ? "LAN" : interfaceFor(draft, "wan")?.port === name ? "WAN" : draft.orchestration_groups.some((group) => group.ports.some((port) => port.interface === name)) ? "编排组" : used ? "聚合成员" : "未使用";
              return (
                <div className="orch-port-row" key={item.id}>
                  <span className="port-icon"><Cable size={15} /></span>
                  <div><strong>{name}</strong><small>{item.speed || "--"} · {item.mac || "--"}</small></div>
                  <Badge tone={role === "管理口" ? "neutral" : role === "LAN" ? "success" : role === "WAN" ? "info" : role === "未使用" ? "neutral" : "warning"}>{role}</Badge>
                </div>
              );
            })}
          </div>
        </Card>
      </div>

      <Card title="编排组" subtitle="每组必须且只能有一个 LAN-facing、一个 WAN-facing 端口；服务链按组顺序经过" actions={<Button size="sm" icon={<Plus size={14} />} onClick={addGroup} disabled={session?.role === "readonly"}>新增编排组</Button>}>
        {draft.orchestration_groups.length ? (
          <div className="orch-group-edit-list">
            {draft.orchestration_groups.map((group, index) => (
              <div className="orch-group-edit" key={`${group.name}-${index}`}>
                <Field label="组名称"><Input value={group.name} onChange={(event) => updateGroup(index, { name: event.target.value })} /></Field>
                <Field label="LAN-facing 端口">
                  <Select value={group.ports.find((port) => port.direction === "lan_facing")?.interface || ""} onChange={(event) => updateGroupPort(index, "lan_facing", event.target.value)}>
                    <option value="">请选择</option>
                    {physicalPorts.map((port) => <option key={port.id} value={port.name || port.id}>{port.name || port.id}</option>)}
                  </Select>
                </Field>
                <Field label="WAN-facing 端口">
                  <Select value={group.ports.find((port) => port.direction === "wan_facing")?.interface || ""} onChange={(event) => updateGroupPort(index, "wan_facing", event.target.value)}>
                    <option value="">请选择</option>
                    {physicalPorts.map((port) => <option key={port.id} value={port.name || port.id}>{port.name || port.id}</option>)}
                  </Select>
                </Field>
                <Button size="sm" variant="text" icon={<Trash2 size={13} />} onClick={() => removeGroup(index)} disabled={session?.role === "readonly"}>移除</Button>
              </div>
            ))}
          </div>
        ) : <p className="muted">暂无编排组；没有编排组时只能做 LAN/WAN 直通策略。</p>}
      </Card>

      <div className="inline-warning"><Network size={14} /> 拓扑保存是同步事务：后端会先下发透明编排，失败时回滚；不会自动创建多站点或跨网关心跳。</div>
    </div>
  );
}
