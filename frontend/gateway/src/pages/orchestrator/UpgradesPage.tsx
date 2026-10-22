import { useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowUpCircle, RefreshCcw, RotateCcw, Upload } from "lucide-react";
import { API } from "../../api/endpoints";
import { apiFetch, postJson } from "../../api/client";
import { useUpgrades } from "../../api/orchestrator-hooks";
import type { UpgradeTask } from "../../api/types";
import { DataTable } from "../../components/DataTable";
import { Badge, Button, Card, Field, PageHeader, useToast } from "../../components/ui";
import { useAuth, useConfirm } from "../../state/providers";
import { formatBytes, formatDateTime, stateLabel, text } from "../../lib/format";

function tone(state: string) {
  if (["installed", "staged"].includes(state)) return "success";
  if (["installing", "rolling_back"].includes(state)) return "warning";
  if (["failed", "rollback_failed"].includes(state)) return "danger";
  return "neutral";
}

export function OrchestratorUpgradesPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const upgrades = useUpgrades();
  const [file, setFile] = useState<File | null>(null);
  const [busy, setBusy] = useState("");
  const version = upgrades.data?.version;
  const tasks = upgrades.data?.items || [];

  const upload = async () => {
    if (!file) { toast("请选择升级包", "error"); return; }
    setBusy("upload");
    try {
      const form = new FormData();
      form.append("package", file, file.name);
      await apiFetch<any>(API.orchestrator.upgrades, { method: "POST", body: form, timeoutMs: 300_000 });
      toast("升级包已上传并暂存", "success");
      setFile(null);
      await queryClient.invalidateQueries({ queryKey: ["orchestrator", "upgrades"] });
    } catch (error: any) {
      toast(error?.message || "升级包上传失败", "error", error?.requestId);
    } finally {
      setBusy("");
    }
  };

  const install = async (task: UpgradeTask) => {
    if (!(await confirm({ title: `安装升级 ${task.to_version || task.id}？`, message: "后端会先返回 202，再异步执行安装；控制服务可能重启。", detail: <span>包：{task.package_name || "--"}<br />任务：{task.id}</span>, confirmLabel: "开始升级" }))) return;
    setBusy(task.id);
    try {
      await postJson(API.orchestrator.upgradeInstall(task.id), { confirm_install: true, reboot: false }, 60_000);
      toast("升级已接受（202），正在异步执行", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator", "upgrades"] });
    } catch (error: any) {
      toast(error?.message || "启动升级失败", "error", error?.requestId);
    } finally {
      setBusy("");
    }
  };

  const rollback = async (task: UpgradeTask) => {
    if (!(await confirm({ title: `回滚 ${task.to_version || task.id}？`, message: "后端会返回 202 并异步恢复备份与 systemd 状态。", confirmLabel: "开始回滚", danger: true }))) return;
    setBusy(task.id);
    try {
      await postJson(API.orchestrator.upgradeRollback(task.id), {}, 60_000);
      toast("回滚已接受（202），正在异步执行", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator", "upgrades"] });
    } catch (error: any) {
      toast(error?.message || "启动回滚失败", "error", error?.requestId);
    } finally {
      setBusy("");
    }
  };

  const columns: ColumnDef<UpgradeTask, any>[] = [
    { id: "id", header: "任务", accessorFn: (row) => row.id, cell: ({ row }) => <span className="mono">{row.original.id.slice(0, 12)}</span>, meta: { width: 120 } },
    { id: "version", header: "版本", accessorFn: (row) => `${row.from_version || "?"} → ${row.to_version || "?"}`, cell: ({ getValue }) => text(getValue()), meta: { width: 190 } },
    { id: "state", header: "状态", accessorFn: (row) => row.state, cell: ({ getValue }) => <Badge tone={tone(String(getValue()))} dot>{stateLabel(getValue())}</Badge>, meta: { width: 110 } },
    { id: "package", header: "升级包", accessorFn: (row) => row.package_name, cell: ({ row }) => <span className="mono truncate" title={row.original.package_name}>{text(row.original.package_name)}</span>, meta: { width: 220 } },
    { id: "size", header: "大小", accessorFn: (row) => row.package_size, cell: ({ getValue }) => formatBytes(getValue()), meta: { width: 90, align: "right" } },
    { id: "updated", header: "更新时间", accessorFn: (row) => row.updated_at, cell: ({ getValue }) => <span className="muted">{formatDateTime(getValue())}</span>, meta: { width: 160 } },
    { id: "reason", header: "原因", accessorFn: (row) => row.reason, cell: ({ getValue }) => <span className="muted truncate" title={text(getValue())}>{text(getValue(), "")}</span> },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions">{row.original.state === "staged" && <Button size="sm" variant="text" icon={<ArrowUpCircle size={13} />} onClick={() => install(row.original)} disabled={session?.role === "readonly" || Boolean(busy)}>安装</Button>}{["installed", "failed", "rollback_failed"].includes(row.original.state) && <Button size="sm" variant="text" icon={<RotateCcw size={13} />} onClick={() => rollback(row.original)} disabled={session?.role === "readonly" || Boolean(busy)}>回滚</Button>}</div>, meta: { width: 90, align: "right", hideable: false } },
  ];

  return (
    <div className="page">
      <PageHeader
        title="升级 / 回滚"
        subtitle="Orchestrator 独立版本；安装/回滚返回 202 后异步执行，需要轮询任务"
        actions={<Button size="sm" icon={<RefreshCcw size={14} />} onClick={() => upgrades.refetch()}>刷新</Button>}
      />

      <div className="config-grid">
        <Card title="当前版本" subtitle="来自 /orchestrator/upgrades">
          <dl className="fact-list">
            <div><dt>产品</dt><dd>{text(version?.product, "orchestrator")}</dd></div>
            <div><dt>版本</dt><dd>{text(version?.version, "--")}</dd></div>
            <div><dt>Commit</dt><dd className="mono">{text(version?.commit, "--")}</dd></div>
            <div><dt>Suite / Arch</dt><dd>{text(version?.suite, "--")} / {text(version?.arch, "--")}</dd></div>
            <div><dt>升级支持</dt><dd><Badge tone={version?.upgrade_supported ? "success" : "warning"}>{version?.upgrade_supported ? "支持" : "不支持"}</Badge></dd></div>
          </dl>
        </Card>

        <Card title="上传升级包" subtitle="multipart 字段 package；仅接受 Orchestrator 升级包">
          <div className="form-stack">
            <Field label="升级包"><input type="file" accept=".tar.zst,.zst" onChange={(event) => setFile(event.target.files?.[0] || null)} /></Field>
            <div className="button-row"><Button variant="primary" icon={<Upload size={14} />} loading={busy === "upload"} onClick={upload} disabled={!file || session?.role === "readonly"}>上传并暂存</Button></div>
            <div className="inline-warning">安装/回滚是 202 异步任务；控制服务重启期间页面可能短暂断连。不要伪造“已完成”。</div>
          </div>
        </Card>
      </div>

      <DataTable data={tasks} columns={columns} loading={upgrades.isLoading} error={upgrades.isError ? upgrades.error : null} onRetry={() => upgrades.refetch()} emptyTitle="暂无升级任务" emptyDescription="上传升级包后会出现在这里。" getRowId={(row) => row.id} />
    </div>
  );
}
