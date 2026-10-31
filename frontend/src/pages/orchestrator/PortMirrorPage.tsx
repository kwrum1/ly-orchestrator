import { useEffect, useMemo, useState } from "react";
import { Cable, Copy, RefreshCw, Trash2 } from "lucide-react";
import { useQueryClient } from "@tanstack/react-query";
import { API } from "../../api/endpoints";
import { patchJson } from "../../api/client";
import { useInterfacesResource } from "../../api/hooks";
import type { InterfaceItem } from "../../api/types";
import { Badge, Button, Card, EmptyState, Field, PageHeader, Select, useToast } from "../../components/ui";
import { useApply, useAuth, useConfirm } from "../../state/providers";
import { useDependencyGuard } from "../../state/dependencies";
import { roleLabel, text } from "../../lib/format";
import { isLogicalPort, takeoverInfo } from "../../lib/human";

// Port mirroring ("镜像口") is an orchestrator capability: one physical data
// port is the mirror destination, any number of other physical data ports are
// mirrored into it. The backend compiles the selection into a VPP SPAN session
// (vpp.interface-span) and proves it with a `show interface span` readback.
// This page only edits the `mirror` / `mirror_sources` attributes of the
// interface resource; it never talks to VPP directly.

const MIRROR_DIRECTIONS = [
  { value: "both", label: "双向" },
  { value: "rx", label: "仅接收" },
  { value: "tx", label: "仅发送" },
];

function mirrorDirectionLabel(direction?: string) {
  return MIRROR_DIRECTIONS.find((item) => item.value === (direction || "both"))?.label || "双向";
}

function interfaceRole(item: InterfaceItem) {
  return item.gateway_role || item.role || (item.mode_role?.gateway as string) || "";
}

function isManagement(item: InterfaceItem) {
  return Boolean((item as any).management) || interfaceRole(item) === "management" || String(item.name || "").toLowerCase() === "eth0";
}

function portKey(item: InterfaceItem) {
  return String(item.id || item.name || "");
}

export function OrchestratorPortMirrorPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const guard = useDependencyGuard();
  const { session } = useAuth();
  const resource = useInterfacesResource();
  const [destination, setDestination] = useState("");
  const [sources, setSources] = useState<Record<string, string>>({});
  const [busy, setBusy] = useState(false);

  const items = (resource.data?.items || []) as InterfaceItem[];
  const ports = useMemo(
    () => items.filter((item) => !isLogicalPort(item) && !Boolean((item as any).__bond)),
    [items]
  );
  const mirrorPort = useMemo(() => ports.find((item) => Boolean(item.mirror)), [ports]);
  const candidates = useMemo(
    () => ports.filter((item) => !isManagement(item) && takeoverInfo(item).assignable),
    [ports]
  );
  const readonly = session?.role === "readonly";

  // Keep the editor in sync with the stored generation (the read path overlays
  // the mirror attributes, so a refresh shows what is actually configured).
  useEffect(() => {
    if (!mirrorPort) {
      setDestination("");
      setSources({});
      return;
    }
    if (destination && destination !== portKey(mirrorPort)) return;
    setDestination(portKey(mirrorPort));
    const next: Record<string, string> = {};
    (mirrorPort.mirror_sources || []).forEach((source) => {
      if (source?.interface) next[String(source.interface)] = source.direction || "both";
    });
    setSources(next);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [mirrorPort?.id, mirrorPort?.name]);

  const sourceDirections = useMemo(() => {
    const map: Record<string, string> = {};
    (mirrorPort?.mirror_sources || []).forEach((source) => {
      if (source?.interface) map[String(source.interface)] = source.direction || "both";
    });
    return map;
  }, [mirrorPort]);

  const refresh = () => {
    void resource.refetch();
  };

  const invalidate = async () => {
    await queryClient.invalidateQueries({ queryKey: ["resources", "interfaces"] });
    await queryClient.invalidateQueries({ queryKey: ["telemetry", "interfaces"] });
  };

  const saveMirror = async () => {
    if (!destination) {
      toast("请先选择镜像目标口", "error");
      return;
    }
    const target = ports.find((item) => portKey(item) === destination);
    const payload = Object.entries(sources).map(([iface, direction]) => ({ interface: iface, direction }));
    setBusy(true);
    await guard.run(async () => {
      await patchJson(API.resources.interface(destination), {
        id: destination,
        name: target?.name || destination,
        mirror: true,
        mirror_sources: payload,
      });
    }, {
      fallback: "保存镜像口失败",
      successMessage: payload.length ? "镜像口已保存" : "镜像口已标记（尚未选择镜像源）",
      onSuccess: async () => {
        await invalidate();
        void apply.apply();
      },
    });
    setBusy(false);
  };

  const clearMirror = async () => {
    if (!mirrorPort) return;
    const target = portKey(mirrorPort);
    const ok = await confirm({
      title: `取消 ${mirrorPort.name || target} 的镜像口？`,
      message: "取消后该口不再接收镜像流量，所有镜像会话会被清除。",
      confirmLabel: "取消镜像",
      danger: true,
    });
    if (!ok) return;
    setBusy(true);
    await guard.run(async () => {
      await patchJson(API.resources.interface(target), {
        id: target,
        name: mirrorPort.name || target,
        mirror: false,
        mirror_sources: [],
      });
    }, {
      fallback: "取消镜像口失败",
      successMessage: "镜像口已取消",
      onSuccess: async () => {
        await invalidate();
        setDestination("");
        setSources({});
        void apply.apply();
      },
    });
    setBusy(false);
  };

  return (
    <div className="page-stack">
      <PageHeader
        title="镜像口"
        subtitle="把一个物理数据口设为镜像目标口，把其它物理口的数据复制到该口用于抓包分析"
        actions={
          <>
            <Button onClick={refresh} loading={resource.isFetching}><RefreshCw size={15} />刷新</Button>
            <Button variant="primary" loading={busy} onClick={saveMirror} disabled={Boolean(readonly)}>
              <Copy size={15} />保存镜像口
            </Button>
          </>
        }
      />

      <Card>
        <div className="notice-block">
          <strong>镜像目标口</strong>
          <span>
            目标口只接收镜像流量，不能同时作为管理口或 LAN/WAN 接口；迁移镜像口前请先取消当前镜像口。
            镜像会话不会改变源口的转发路径，目标口无载波时仅告警。
          </span>
        </div>
      </Card>

      <Card title="当前状态" actions={mirrorPort && <Badge tone="info">镜像口：{mirrorPort.name || portKey(mirrorPort)}</Badge>}>
        {!mirrorPort && <EmptyState title="尚未配置镜像口" description="选择下方的一个物理口作为镜像目标口，并勾选需要镜像的源口。" />}
        {mirrorPort && (
          <div className="form-stack">
            <div className="notice-block">
              <strong>{mirrorPort.name || portKey(mirrorPort)}</strong>
              <span>
                链路 {text(mirrorPort.link_state) || "未知"} · MAC {text(mirrorPort.mac) || "无"} ·
                {" "}{Math.max((mirrorPort.mirror_sources || []).length, 0)} 个镜像源
              </span>
            </div>
            <div className="check-grid">
              {ports.filter((item) => sourceDirections[portKey(item)]).map((item) => (
                <div className="check-card" key={portKey(item)}>
                  <span>
                    <strong>{item.name || portKey(item)}</strong>
                    <small>镜像源 · {mirrorDirectionLabel(sourceDirections[portKey(item)])}</small>
                  </span>
                </div>
              ))}
              {!(mirrorPort.mirror_sources || []).length && <div className="inline-warning">镜像口还没有镜像源。</div>}
            </div>
            <div>
              <Button variant="danger" onClick={clearMirror} disabled={busy || Boolean(readonly)}>
                <Trash2 size={15} />取消镜像
              </Button>
            </div>
          </div>
        )}
      </Card>

      <Card title="编辑镜像配置">
        <div className="form-stack">
          <Field label="镜像目标口" hint="只能选择一个物理数据口；已接管且未配置 LAN/WAN 角色的口才能作为目标口。">
            <Select
              value={destination}
              disabled={Boolean(readonly)}
              onChange={(event) => setDestination(event.target.value)}
            >
              <option value="">未选择</option>
              {candidates.map((item) => (
                <option key={portKey(item)} value={portKey(item)}>
                  {item.name || portKey(item)}{item.mirror ? "（当前镜像口）" : ""}
                </option>
              ))}
            </Select>
          </Field>

          {destination && (
            <Field label="镜像源" hint="勾选需要镜像到目标口的其它物理口；LAN/WAN 口也可以作为镜像源，方向默认双向。">
              <div className="check-grid">
                {candidates.filter((item) => portKey(item) !== destination).map((item) => {
                  const key = portKey(item);
                  const checked = Object.prototype.hasOwnProperty.call(sources, key);
                  return (
                    <label className="check-card" key={key}>
                      <input
                        type="checkbox"
                        checked={checked}
                        disabled={Boolean(readonly)}
                        onChange={(event) => setSources((current) => {
                          const next = { ...current };
                          if (event.target.checked) next[key] = next[key] || "both";
                          else delete next[key];
                          return next;
                        })}
                      />
                      <span>
                        <strong>{item.name || key}</strong>
                        <small>{roleLabel(interfaceRole(item)) || "未配置"}</small>
                      </span>
                      {checked && (
                        <Select
                          value={sources[key] || "both"}
                          onClick={(event) => event.stopPropagation()}
                          onChange={(event) => {
                            event.stopPropagation();
                            setSources((current) => ({ ...current, [key]: event.target.value }));
                          }}
                        >
                          {MIRROR_DIRECTIONS.map((direction) => (
                            <option key={direction.value} value={direction.value}>{direction.label}</option>
                          ))}
                        </Select>
                      )}
                    </label>
                  );
                })}
              </div>
            </Field>
          )}

          {!candidates.length && <div className="inline-warning">没有可用作镜像口/镜像源的已接管物理网卡。</div>}
          <div className="inline-warning">
            <Cable size={15} />
            {" "}保存后会立即应用；镜像会话由 VPP 承载，并在应用回执中用 <code>show interface span</code> 校验。
          </div>
        </div>
      </Card>
    </div>
  );
}
