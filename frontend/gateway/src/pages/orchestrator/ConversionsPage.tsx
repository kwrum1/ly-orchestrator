import { useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { useQueryClient } from "@tanstack/react-query";
import { ArrowLeftRight, CheckCircle2, FileJson, Play, RefreshCw, RotateCcw, ShieldAlert } from "lucide-react";
import { API } from "../../api/endpoints";
import { postJson } from "../../api/client";
import { useConversions } from "../../api/orchestrator-hooks";
import type { ConversionTask, ConversionValidation } from "../../api/types";
import { DataTable } from "../../components/DataTable";
import { Badge, Button, Card, Field, PageHeader, useToast } from "../../components/ui";
import { useAuth, useConfirm } from "../../state/providers";
import { formatDateTime, stateLabel, text } from "../../lib/format";

function tone(state: string) {
  if (["validated", "ready", "applied"].includes(state)) return "success";
  if (["applying"].includes(state)) return "warning";
  if (["failed"].includes(state)) return "danger";
  return "neutral";
}

export function OrchestratorConversionsPage() {
  const queryClient = useQueryClient();
  const { toast } = useToast();
  const confirm = useConfirm();
  const { session } = useAuth();
  const conversions = useConversions();
  const [manifestFile, setManifestFile] = useState<File | null>(null);
  const [validation, setValidation] = useState<ConversionValidation | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState("");
  const [blocked, setBlocked] = useState("");
  const tasks = conversions.data?.items || [];

  const readManifest = async () => {
    if (!manifestFile) throw new Error("请选择 conversion manifest JSON");
    if (manifestFile.size > 1 * 1024 * 1024) throw new Error("manifest 不能超过 1 MiB");
    return JSON.parse(await manifestFile.text());
  };

  const validate = async () => {
    setBusy("validate");
    setError("");
    setBlocked("");
    setValidation(null);
    try {
      const manifest = await readManifest();
      const result = await postJson<any>(API.orchestrator.conversionValidate, { manifest }, 30_000);
      setValidation(result.validation || result);
      toast("manifest 校验完成", "success");
    } catch (failed: any) {
      const bodyValidation = failed?.body?.validation;
      if (bodyValidation) setValidation(bodyValidation);
      setError(failed?.message || "manifest 校验失败");
    } finally {
      setBusy("");
    }
  };

  const create = async () => {
    setBusy("create");
    setError("");
    try {
      const manifest = await readManifest();
      await postJson<any>(API.orchestrator.conversions, { manifest, prepare: true }, 30_000);
      toast("转换任务已创建", "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator", "conversions"] });
    } catch (failed: any) {
      if (failed?.body?.validation) setValidation(failed.body.validation);
      setError(failed?.message || "创建转换任务失败");
    } finally {
      setBusy("");
    }
  };

  const action = async (task: ConversionTask, kind: "prepare" | "rollback" | "apply") => {
    if (kind === "apply") {
      const ok = await confirm({ title: "执行转换 apply？", message: "当前发行版明确未实现真实镜像转换；后端预期返回 501 conversion_execution_blocked。", confirmLabel: "请求 apply", danger: true });
      if (!ok) return;
    }
    setBusy(`${kind}-${task.id}`);
    setError("");
    setBlocked("");
    try {
      const path = kind === "prepare" ? API.orchestrator.conversionPrepare(task.id) : kind === "rollback" ? API.orchestrator.conversionRollback(task.id) : API.orchestrator.conversionApply(task.id);
      await postJson<any>(path, {}, 30_000);
      toast(`${kind} 已接受`, "success");
      await queryClient.invalidateQueries({ queryKey: ["orchestrator", "conversions"] });
    } catch (failed: any) {
      if (kind === "apply" && failed?.status === 501) {
        setBlocked("后端未实现真实镜像转换：501 conversion_execution_blocked。UI 不会把它显示为成功。");
      } else {
        setError(failed?.message || `${kind} 失败`);
      }
    } finally {
      setBusy("");
    }
  };

  const columns: ColumnDef<ConversionTask, any>[] = [
    { id: "id", header: "任务", accessorFn: (row) => row.id, cell: ({ row }) => <span className="mono">{row.original.id.slice(0, 12)}</span>, meta: { width: 120 } },
    { id: "product", header: "方向", accessorFn: (row) => `${row.from_product || "?"} → ${row.to_product || "?"}`, cell: ({ getValue }) => text(getValue()), meta: { width: 180 } },
    { id: "version", header: "版本", accessorFn: (row) => `${row.from_version || "?"} → ${row.to_version || "?"}`, cell: ({ getValue }) => text(getValue()), meta: { width: 190 } },
    { id: "state", header: "状态", accessorFn: (row) => row.state, cell: ({ getValue }) => <Badge tone={tone(String(getValue()))} dot>{stateLabel(getValue())}</Badge>, meta: { width: 110 } },
    { id: "snapshot", header: "快照", accessorFn: (row) => row.snapshot_id, cell: ({ getValue }) => <span className="mono">{text(getValue(), "—")}</span>, meta: { width: 120 } },
    { id: "updated", header: "更新时间", accessorFn: (row) => row.updated_at, cell: ({ getValue }) => <span className="muted">{formatDateTime(getValue())}</span>, meta: { width: 160 } },
    { id: "actions", header: "操作", cell: ({ row }) => <div className="row-actions"><Button size="sm" variant="text" icon={<CheckCircle2 size={13} />} onClick={() => action(row.original, "prepare")} disabled={session?.role === "readonly" || Boolean(busy)}>准备</Button><Button size="sm" variant="text" icon={<Play size={13} />} onClick={() => action(row.original, "apply")} disabled={session?.role === "readonly" || Boolean(busy)}>Apply</Button><Button size="sm" variant="text" icon={<RotateCcw size={13} />} onClick={() => action(row.original, "rollback")} disabled={session?.role === "readonly" || Boolean(busy)}>回滚</Button></div>, meta: { width: 170, align: "right", hideable: false } },
  ];

  const plan = validation?.plan;
  return (
    <div className="page">
      <PageHeader
        title="产品转换"
        subtitle="Gateway ↔ Orchestrator 的 manifest 校验、任务、准备与回滚；真实镜像转换未实现"
        actions={<Button size="sm" icon={<RefreshCw size={14} />} onClick={() => conversions.refetch()}>刷新</Button>}
      />

      <div className="inline-warning"><ShieldAlert size={14} /> 当前发行版只实现转换骨架：校验、任务持久化、配置快照、服务 enable 状态捕获/恢复与回滚。<code>POST /conversions/[id]/apply</code> 返回 <code>501 conversion_execution_blocked</code>，UI 明确显示，不伪造成功。</div>

      <div className="config-grid">
        <Card title="上传 conversion manifest" subtitle="JSON；不接受镜像上传（后端没有上传端点）" actions={<Badge tone="info">ly-route-conversion</Badge>}>
          <div className="form-stack">
            <Field label="Manifest 文件"><input type="file" accept="application/json,.json" onChange={(event) => { setManifestFile(event.target.files?.[0] || null); setValidation(null); setError(""); }} /></Field>
            <div className="button-row"><Button icon={<CheckCircle2 size={14} />} loading={busy === "validate"} onClick={validate} disabled={!manifestFile || session?.role === "readonly"}>校验</Button><Button variant="primary" icon={<FileJson size={14} />} loading={busy === "create"} onClick={create} disabled={!manifestFile || session?.role === "readonly"}>创建任务（prepare）</Button></div>
            {error && <div className="error-state"><strong>操作失败</strong><p>{error}</p></div>}
            {blocked && <div className="inline-warning">{blocked}</div>}
          </div>
        </Card>

        <Card title="校验结果" subtitle={validation?.valid ? "manifest 可用于创建任务" : "尚未校验"}>
          {validation ? (
            <div className="conversion-validation">
              <Badge tone={validation.valid ? "success" : "danger"} dot>{validation.valid ? "valid" : "invalid"}</Badge>
              {validation.issues?.length ? <ul className="warning-list">{validation.issues.map((issue, index) => <li key={index}>{issue}</li>)}</ul> : <p className="muted">无校验问题。</p>}
              {plan && <dl className="fact-list"><div><dt>方向</dt><dd>{plan.from_product} → {plan.to_product}</dd></div><div><dt>Dataplane</dt><dd>{text(plan.dataplane_mode)}</dd></div><div><dt>配置迁移</dt><dd>{text(plan.config_migration)}</dd></div><div><dt>快照要求</dt><dd>{plan.snapshot_required ? "需要" : "不需要"}</dd></div><div><dt>回滚</dt><dd>{plan.rollback_supported ? "支持" : "不支持"}</dd></div><div><dt>步骤</dt><dd>{plan.steps?.length || 0}</dd></div></dl>}
            </div>
          ) : <p className="muted">选择 manifest 后点击“校验”。</p>}
        </Card>
      </div>

      <DataTable data={tasks} columns={columns} loading={conversions.isLoading} error={conversions.isError ? conversions.error : null} onRetry={() => conversions.refetch()} emptyTitle="暂无转换任务" emptyDescription="校验并创建任务后会显示在这里。" getRowId={(row) => row.id} />
    </div>
  );
}
