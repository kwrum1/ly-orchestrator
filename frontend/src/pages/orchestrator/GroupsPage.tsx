import { useEffect, useMemo, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Plus, RefreshCw, Save, Trash2, Waypoints } from "lucide-react";
import { API } from "../../api/endpoints";
import { putJson } from "../../api/client";
import { useInterfacesResource } from "../../api/hooks";
import { useOrchestratorTopology } from "../../api/orchestrator-hooks";
import type { OrchestrationGroup, Topology } from "../../api/types";
import { Badge, Button, Card, Field, Input, PageHeader, Select, useToast } from "../../components/ui";
import { useAuth } from "../../state/providers";
import { takeoverInfo } from "../../lib/human";

type GroupDraft = { name: string; lan: string; wan: string };

function normalizedDirection(value: unknown) {
  return String(value || "").trim().toLowerCase().replace(/-/g, "_");
}

function portOf(group: OrchestrationGroup | null | undefined, direction: "lan_facing" | "wan_facing") {
  if (!group) return "";
  const port = (group.ports || []).find((item) => normalizedDirection(item.direction) === direction);
  return port?.interface || port?.bond?.name || "";
}

function draftsFrom(groups: OrchestrationGroup[]): GroupDraft[] {
  return groups.map((group) => ({
    name: group.name,
    lan: portOf(group, "lan_facing"),
    wan: portOf(group, "wan_facing"),
  }));
}

function topologyPayload(topology: Topology, groups: GroupDraft[]) {
  return {
    schema_version: topology.schema_version || 1,
    management_interface: topology.management_interface,
    interfaces: (topology.interfaces || []).map((item) => item.bond?.members?.length
      ? { name: item.name, role: item.role, bond: item.bond }
      : { name: item.name, role: item.role, port: item.port }),
    port_roles: topology.port_roles || {},
    orchestration_groups: groups.map((group) => ({
      name: group.name,
      ports: [
        { interface: group.lan, direction: "lan_facing" },
        { interface: group.wan, direction: "wan_facing" },
      ],
    })),
  };
}

export function OrchestratorGroupsPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const { session } = useAuth();
  const topology = useOrchestratorTopology();
  const inventory = useInterfacesResource();
  const [drafts, setDrafts] = useState<GroupDraft[] | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const readonly = session?.role === "readonly";

  const groups = topology.data?.item?.orchestration_groups || [];
  const management = topology.data?.item?.management_interface || "";

  useEffect(() => {
    if (topology.data?.item && drafts === null) {
      setDrafts(draftsFrom(topology.data.item.orchestration_groups || []));
    }
  }, [topology.data, drafts]);

  const candidates = useMemo(() => {
    const values = new Map<string, string>();
    for (const item of inventory.data?.items || []) {
      const value = item.system_name || item.name || item.id;
      if (!value || value === management) continue;
      if (!takeoverInfo(item).assignable) continue;
      const label = (item.system_name && item.name && item.name !== item.system_name
        ? `${item.name} · ${item.system_name}`
        : (item.name || item.id || item.system_name)) || value;
      values.set(value, label);
    }
    for (const group of groups) {
      for (const port of group.ports || []) {
        if (port.interface) values.set(port.interface, values.get(port.interface) || port.interface);
      }
    }
    return [...values.entries()];
  }, [inventory.data, groups, management]);

  const update = (index: number, patch: Partial<GroupDraft>) => {
    if (!drafts) return;
    setDrafts(drafts.map((draft, position) => position === index ? { ...draft, ...patch } : draft));
  };

  const add = () => {
    if (!drafts) return;
    setDrafts([...drafts, { name: `grp-${drafts.length + 1}`, lan: "", wan: "" }]);
  };

  const save = async () => {
    if (!drafts || !topology.data?.item) return;
    setError("");
    const names = new Set<string>();
    for (const draft of drafts) {
      const name = draft.name.trim();
      if (!name) { setError("请填写每个编排组的名称"); return; }
      if (names.has(name)) { setError(`编排组名称重复：${name}`); return; }
      names.add(name);
      if (!draft.lan || !draft.wan) { setError(`编排组 ${name} 需要 LAN-facing 与 WAN-facing 端口`); return; }
      if (draft.lan === draft.wan) { setError(`编排组 ${name} 的 LAN/WAN 端口不能相同`); return; }
    }
    setBusy(true);
    try {
      const payload = topologyPayload(topology.data.item, drafts.map((draft) => ({ ...draft, name: draft.name.trim() })));
      const result = await putJson<{ item: Topology }>(API.orchestrator.topology, payload, 60_000);
      setDrafts(draftsFrom(result?.item?.orchestration_groups || []));
      toast("编排组已保存", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator"] });
      await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
    } catch (failed: any) {
      const message = failed?.message || "保存编排组失败";
      setError(message);
      toast(message, "error");
    } finally {
      setBusy(false);
    }
  };

  const reset = () => {
    setError("");
    setDrafts(topology.data?.item ? draftsFrom(topology.data.item.orchestration_groups || []) : []);
  };

  if (topology.isLoading || drafts === null) return <div className="page"><Card title="编排组"><div className="muted">正在读取…</div></Card></div>;

  return (
    <div className="page">
      <PageHeader
        title="编排组"
        subtitle="每组两个方向端口；策略 via 动作按组进入服务链。新增、编辑、删除均在当前页表单内完成。"
        actions={
          <>
            <Button size="sm" icon={<RefreshCw size={14} />} onClick={reset} disabled={busy}>刷新</Button>
            <Button size="sm" icon={<Plus size={14} />} onClick={add} disabled={readonly || !topology.data?.item}>新增编排组</Button>
            <Button size="sm" variant="primary" icon={<Save size={14} />} loading={busy} onClick={save} disabled={readonly || !topology.data?.item}>保存编排组</Button>
          </>
        }
      />
      {!topology.data?.item && <div className="inline-warning">请先在“网卡设置”中配置 LAN/WAN 并保存，再创建编排组。</div>}
      {error && <div className="error-state" role="alert"><strong>保存失败</strong><p>{error}</p></div>}

      <Card title="编排组" subtitle="直接编辑下方表单；保存会 PUT 拓扑并立即应用">
        {!drafts.length && <div className="inline-warning">暂无编排组。没有编排组时只能做 LAN/WAN 直通策略。</div>}
        <div className="orch-group-edit-list">
          {drafts.map((draft, index) => (
            <div className="orch-group-edit" key={`${index}-${draft.name}`}>
              <Field label="组名称">
                <Input value={draft.name} disabled={readonly} onChange={(event) => update(index, { name: event.target.value })} />
              </Field>
              <Field label="LAN-facing 端口">
                <Select value={draft.lan} disabled={readonly} onChange={(event) => update(index, { lan: event.target.value })}>
                  <option value="">请选择</option>
                  {candidates.map(([value, label]) => <option key={`lan-${index}-${value}`} value={value}>{label}</option>)}
                </Select>
              </Field>
              <Field label="WAN-facing 端口">
                <Select value={draft.wan} disabled={readonly} onChange={(event) => update(index, { wan: event.target.value })}>
                  <option value="">请选择</option>
                  {candidates.map(([value, label]) => <option key={`wan-${index}-${value}`} value={value}>{label}</option>)}
                </Select>
              </Field>
              <Button size="sm" variant="text" icon={<Trash2 size={13} />} disabled={readonly} onClick={() => setDrafts(drafts.filter((_, position) => position !== index))}>移除</Button>
            </div>
          ))}
        </div>
        <div className="inline-warning" style={{ marginTop: 12 }}>
          <Waypoints size={14} /> 编排组不能使用管理口、逻辑 LAN/WAN 端口或已占用端口；点击“保存编排组”后由后端校验并应用。
        </div>
      </Card>
    </div>
  );
}
