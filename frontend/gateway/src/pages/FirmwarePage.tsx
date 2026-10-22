import { useState } from "react";
import { HardDrive, RefreshCw, Upload, Zap } from "lucide-react";
import { API } from "../api/endpoints";
import { apiFetch, postJson } from "../api/client";
import { useFirmwareStatus } from "../api/hooks";
import { Badge, Button, Card, Field, Input, Modal, PageHeader, useToast } from "../components/ui";
import { useAuth, useConfirm } from "../state/providers";
import { formatDateTime, stateLabel, text } from "../lib/format";

export function FirmwarePage() {
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const status = useFirmwareStatus();
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState("");
  const [installOpen, setInstallOpen] = useState(false);

  const stage = async () => {
    if (!file) { toast("请选择升级包", "error"); return; }
    setBusy("stage");
    try {
      const form = new FormData();
      form.append("firmware", file, file.name);
      const result = await apiFetch<any>(API.firmware.stage, { method: "POST", body: form, timeoutMs: 180_000 });
      toast("升级包已上传并校验通过", "success");
      await status.refetch();
      return result;
    } catch (error: any) {
      toast(error?.message || "固件上传失败", "error", error?.requestId);
    } finally {
      setBusy("");
    }
  };

  const install = async () => {
    setBusy("install");
    setInstallOpen(false);
    try {
      await postJson(API.firmware.install, { confirm_install: true, target_dir: "/usr/lib/ly-route", reboot: false }, 60_000);
      toast("升级已启动，控制服务将重启", "success");
      await status.refetch();
    } catch (error: any) {
      toast(error?.message || "固件升级启动失败", "error", error?.requestId);
    } finally {
      setBusy("");
    }
  };

  const data = status.data || {};
  const staged = Boolean(data.staged || data.status === "staged" || data.status === "validated");

  return (
    <div className="page">
      <PageHeader
        title="固件升级"
        subtitle="上传、校验、安装；升级过程中控制服务可能重启"
        actions={<Button size="sm" icon={<RefreshCw size={14} />} onClick={() => status.refetch()}>刷新状态</Button>}
      />
      <div className="config-grid">
        <Card title="升级状态" subtitle="来自 /firmware/update/status" actions={<Badge tone={staged ? "success" : "neutral"} dot>{staged ? "已校验" : stateLabel(data.status || "idle")}</Badge>}>
          <dl className="fact-list">
            <div><dt>状态</dt><dd>{text(data.status, "idle")}</dd></div>
            <div><dt>包名</dt><dd className="mono">{text(data.image || data.filename || data.file, "--")}</dd></div>
            <div><dt>校验</dt><dd>{data.sha256 ? String(data.sha256).slice(0, 16) : "--"}</dd></div>
            <div><dt>更新时间</dt><dd>{formatDateTime(data.updated_at || data.staged_at)}</dd></div>
            <div><dt>重启</dt><dd>{data.reboot ? "需要" : "不自动重启"}</dd></div>
          </dl>
          {data.reason && <div className="inline-warning">{data.reason}</div>}
        </Card>

        <Card title="上传升级包" subtitle="接受 .tar.zst / .zst；上传后会校验">
          <div className="form-stack">
            <Field label="升级包" required><input type="file" accept=".tar.zst,.zst" onChange={(event) => setFile(event.target.files?.[0] || null)} /></Field>
            <div className="button-row">
              <Button variant="primary" icon={<Upload size={14} />} loading={busy === "stage"} onClick={stage} disabled={!file || session?.role === "readonly"}>上传并校验</Button>
              <Button variant="secondary" icon={<Zap size={14} />} onClick={() => setInstallOpen(true)} disabled={!staged || session?.role === "readonly" || Boolean(busy)}>开始升级</Button>
            </div>
            <div className="inline-warning"><HardDrive size={14} /> 升级会重启控制服务；请确认升级包与设备架构匹配，并已导出配置备份。</div>
          </div>
        </Card>
      </div>

      <Modal open={installOpen} onClose={() => setInstallOpen(false)} title="确认开始升级" size="sm" footer={<><Button onClick={() => setInstallOpen(false)}>取消</Button><Button variant="danger" loading={busy === "install"} onClick={install}>确认升级</Button></>}>
        <div className="form-stack">
          <p>升级过程中控制服务会重启，管理界面可能短暂不可用。</p>
          <div className="inline-warning">请勿断电，并保留回滚包/配置备份。</div>
        </div>
      </Modal>
    </div>
  );
}
