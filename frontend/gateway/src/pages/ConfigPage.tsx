import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { Clock3, Download, HardDrive, RotateCcw, Save, ServerCog, ShieldAlert, Trash2 } from "lucide-react";
import { API } from "../api/endpoints";
import { getJson, postJson } from "../api/client";
import { useHealth, useInterfacesResource, useManagementNetwork, useSummary } from "../api/hooks";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, Select, useToast } from "../components/ui";
import { useApply, useAuth, useConfirm } from "../state/providers";
import { formatDateTime, formatDuration, stateLabel, text } from "../lib/format";

export function ConfigPage() {
  const navigate = useNavigate();
  const { toast } = useToast();
  const confirm = useConfirm();
  const apply = useApply();
  const { session } = useAuth();
  const summary = useSummary();
  const health = useHealth();
  const management = useManagementNetwork();
  const interfaces = useInterfacesResource();
  const [form, setForm] = useState<any>({});
  const [busy, setBusy] = useState(false);
  const [snapshots, setSnapshots] = useState<any[]>([]);
  const [snapshotName, setSnapshotName] = useState("");
  const [resetOpen, setResetOpen] = useState(false);
  const [resetText, setResetText] = useState("");

  useEffect(() => {
    const item = management.data?.item || management.data || {};
    setForm({
      interfaceId: item.interface_id || "",
      cidr: item.cidr || item.ip_cidr || "",
      gateway: item.gateway || "",
      mode: item.mode || "exclusive",
    });
  }, [management.data]);

  useEffect(() => {
    void loadSnapshots();
  }, []);

  const loadSnapshots = async () => {
    try {
      const result = await getJson<any>(API.config.snapshots);
      setSnapshots(result?.items || []);
    } catch {
      setSnapshots([]);
    }
  };

  const saveManagement = async () => {
    if (!form.interfaceId || !form.cidr) { toast("请选择管理接口并填写 IP/掩码", "error"); return; }
    const ok = await confirm({
      title: "保存管理口设置？",
      message: "修改管理口可能导致当前连接中断，请确认你已准备好通过新地址重新连接。",
      detail: <span>新地址：{form.cidr}；网关：{form.gateway || "无"}</span>,
      confirmLabel: "保存",
    });
    if (!ok) return;
    setBusy(true);
    try {
      await postJson(API.management, { interface_id: form.interfaceId, cidr: form.cidr, gateway: form.gateway, mode: form.mode });
      toast("管理口已保存，正在应用", "success");
      await management.refetch();
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "保存管理口失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const exportConfig = async () => {
    try {
      const result = await getJson<any>(API.config.export);
      const blob = new Blob([JSON.stringify(result, null, 2)], { type: "application/json" });
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = `ly-route-config-${new Date().toISOString().slice(0, 10)}.json`;
      link.click();
      URL.revokeObjectURL(url);
      toast("配置已导出", "success");
    } catch (error: any) {
      toast(error?.message || "导出配置失败", "error", error?.requestId);
    }
  };

  const createSnapshot = async () => {
    if (!snapshotName.trim()) { toast("请填写快照名称", "error"); return; }
    try {
      await postJson(API.config.snapshots, { name: snapshotName.trim() });
      setSnapshotName("");
      await loadSnapshots();
      toast("快照已创建", "success");
    } catch (error: any) {
      toast(error?.message || "创建快照失败", "error", error?.requestId);
    }
  };

  const restoreSnapshot = async (snapshot: any) => {
    const ok = await confirm({ title: `恢复快照 ${snapshot.name || snapshot.id}？`, message: "当前期望配置会被替换；恢复后需要应用运行态。", confirmLabel: "恢复", danger: true });
    if (!ok) return;
    try {
      await postJson(API.config.snapshot(snapshot.id), {});
      toast("快照已恢复，正在应用", "success");
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "恢复快照失败", "error", error?.requestId);
    }
  };

  const factoryReset = async () => {
    if (resetText !== "RESET") { toast("请输入 RESET 确认", "error"); return; }
    setBusy(true);
    try {
      await postJson(API.config.factoryReset, {});
      toast("配置初始化已执行", "success");
      setResetOpen(false);
      setResetText("");
      void apply.apply();
    } catch (error: any) {
      toast(error?.message || "配置初始化失败", "error", error?.requestId);
    } finally {
      setBusy(false);
    }
  };

  const system = summary.data?.system || {};
  const candidates = (interfaces.data?.items || []).filter((item) => (item.gateway_role || item.role) !== "wan");
  const managementOptions = form.interfaceId && !candidates.some((item) => item.id === form.interfaceId)
    ? [{ id: form.interfaceId, name: form.interfaceId }, ...candidates]
    : candidates;

  return (
    <div className="page">
      <PageHeader
        title="配置管理"
        subtitle="管理口、系统状态、配置备份与恢复"
        actions={<Button size="sm" variant="primary" icon={<ServerCog size={14} />} onClick={() => navigate("/system/runtime")}>运行态</Button>}
      />

      <div className="config-grid">
        <Card title="管理口设置" subtitle="管理地址变更可能中断当前连接">
          <div className="form-grid">
            <Field label="管理接口" required><Select value={form.interfaceId || ""} onChange={(event) => setForm({ ...form, interfaceId: event.target.value })}><option value="">请选择接口</option>{managementOptions.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}</Select></Field>
            <Field label="IP/掩码" required><Input value={form.cidr || ""} onChange={(event) => setForm({ ...form, cidr: event.target.value })} placeholder="192.168.88.254/24" /></Field>
            <Field label="网关"><Input value={form.gateway || ""} onChange={(event) => setForm({ ...form, gateway: event.target.value })} placeholder="可选" /></Field>
            <Field label="模式"><Select value={form.mode || "exclusive"} onChange={(event) => setForm({ ...form, mode: event.target.value })}><option value="exclusive">独立管理口</option><option value="shared_lan">与 LAN 共享</option></Select></Field>
          </div>
          <div className="card-actions-bottom"><Button variant="primary" loading={busy} onClick={saveManagement} disabled={session?.role === "readonly"}>保存管理口</Button></div>
        </Card>

        <Card title="系统信息" subtitle="来自 /dashboard/summary">
          <dl className="fact-list">
            <div><dt>版本</dt><dd>{text(health.data?.version, "--")}</dd></div>
            <div><dt>运行时长</dt><dd>{formatDuration(system.uptime_seconds)}</dd></div>
            <div><dt>系统时间</dt><dd>{formatDateTime(system.system_time)}</dd></div>
            <div><dt>硬件平台</dt><dd>{text(system.platform, "--")}</dd></div>
            <div><dt>CPU / 内存</dt><dd>{system.cpu_busy_percent ?? "--"}% / {system.memory_used_percent ?? "--"}%</dd></div>
          </dl>
          <div className="inline-warning">系统时间当前仅支持读取；后端没有提供时间写入 API，因此不提供“同步时间”假按钮。</div>
        </Card>

        <Card title="配置备份" subtitle="导出期望配置 JSON；导入/快照恢复需产品确认后开放">
          <div className="button-row">
            <Button icon={<Download size={14} />} onClick={exportConfig}>导出配置</Button>
            <Button icon={<Save size={14} />} onClick={() => navigate("/system/runtime")}>查看运行态</Button>
          </div>
          <div className="snapshot-create">
            <Input value={snapshotName} onChange={(event) => setSnapshotName(event.target.value)} placeholder="快照名称" />
            <Button size="sm" onClick={createSnapshot} disabled={session?.role === "readonly"}>创建快照</Button>
          </div>
          <div className="snapshot-list">
            {snapshots.slice(0, 6).map((snapshot) => (
              <div className="snapshot-row" key={snapshot.id}>
                <div><strong>{text(snapshot.name || snapshot.id)}</strong><small>{formatDateTime(snapshot.created_at || snapshot.timestamp)}</small></div>
                <Badge tone="neutral">{text(snapshot.reason || snapshot.kind || "snapshot")}</Badge>
                <Button size="sm" variant="ghost" icon={<RotateCcw size={13} />} onClick={() => restoreSnapshot(snapshot)} disabled={session?.role === "readonly"}>恢复</Button>
              </div>
            ))}
            {!snapshots.length && <p className="muted">暂无快照。</p>}
          </div>
        </Card>

        <Card title="危险操作" subtitle="配置初始化会清空期望配置并保留管理员账号/管理口">
          <div className="danger-zone">
            <ShieldAlert size={18} />
            <div><strong>配置初始化</strong><p>会创建回滚快照并恢复默认配置；操作后需要应用运行态。</p></div>
            <Button variant="danger" icon={<Trash2 size={14} />} onClick={() => setResetOpen(true)} disabled={session?.role === "readonly"}>配置初始化</Button>
          </div>
        </Card>
      </div>

      <Modal
        open={resetOpen}
        onClose={() => !busy && setResetOpen(false)}
        title="确认配置初始化"
        size="sm"
        busy={busy}
        footer={<><Button onClick={() => setResetOpen(false)} disabled={busy}>取消</Button><Button variant="danger" loading={busy} onClick={factoryReset}>确认初始化</Button></>}
      >
        <div className="form-stack">
          <div className="inline-warning">该操作会清空当前配置。请输入 <code>RESET</code> 确认。</div>
          <Field label="确认文本"><Input value={resetText} onChange={(event) => setResetText(event.target.value)} placeholder="RESET" /></Field>
        </div>
      </Modal>
    </div>
  );
}
