import { useState } from "react";
import { Play, RotateCcw } from "lucide-react";
import { Button, Card, Field, Input, PageHeader, Select, useToast } from "../components/ui";

export function DiagnosticsPage() {
  const { toast } = useToast();
  const [target, setTarget] = useState("223.5.5.5");
  const [count, setCount] = useState("4");
  const [result, setResult] = useState("");
  const [busy, setBusy] = useState(false);

  const run = () => {
    if (!target.trim()) { toast("请输入 IP 或域名", "error"); return; }
    setBusy(true);
    setResult("");
    // 后端 Ping 接口接入前保持明确的空态，不伪造结果
    window.setTimeout(() => {
      setBusy(false);
      setResult("诊断接口待接入。当前页面仅保留 Ping 入口。");
    }, 300);
  };

  return (
    <div className="page">
      <PageHeader title="网络诊断" subtitle="单次 Ping，用于快速确认连通性" />
      <Card title="Ping" subtitle="目标 IP 或域名">
        <div className="form-grid">
          <Field label="目标"><Input value={target} onChange={(event) => setTarget(event.target.value)} placeholder="例如 223.5.5.5" /></Field>
          <Field label="次数"><Select value={count} onChange={(event) => setCount(event.target.value)}><option>4</option><option>8</option><option>16</option></Select></Field>
        </div>
        <div className="row-actions" style={{ marginTop: 12 }}>
          <Button variant="primary" icon={<Play size={14} />} loading={busy} onClick={run}>开始</Button>
          <Button icon={<RotateCcw size={14} />} onClick={() => { setResult(""); setTarget("223.5.5.5"); }}>重置</Button>
        </div>
        <pre className="diagnostic-output">{result || "尚未执行。"}</pre>
      </Card>
    </div>
  );
}
