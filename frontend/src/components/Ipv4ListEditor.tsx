import { Plus, Trash2 } from "lucide-react";
import { Button, Input } from "./ui";

const IPV4 = /^(?:(?:25[0-5]|2[0-4]\d|1?\d?\d)\.){3}(?:25[0-5]|2[0-4]\d|1?\d?\d)$/;

/** Validate a trimmed IPv4 allow-list, preserving the operator's order. */
export function validateIpv4List(values: string[]): string | null {
  const seen = new Set<string>();
  for (const raw of values) {
    const value = String(raw || "").trim();
    if (!value) continue;
    if (!IPV4.test(value)) return `“${value}”不是有效的 IPv4 地址`;
    if (seen.has(value)) return `地址 ${value} 重复，请删除重复项`;
    seen.add(value);
  }
  return null;
}

export function Ipv4ListEditor({
  value,
  onChange,
  placeholder = "203.0.113.10",
  disabled,
  hint,
}: {
  value: string[];
  onChange: (next: string[]) => void;
  placeholder?: string;
  disabled?: boolean;
  hint?: string;
}) {
  const rows = value.length ? value : [""];
  const update = (index: number, next: string) => {
    const copy = [...rows];
    copy[index] = next;
    onChange(copy);
  };
  const remove = (index: number) => onChange(rows.filter((_, current) => current !== index));
  return (
    <div className="ipv4-list">
      {rows.map((item, index) => {
        const trimmed = String(item || "").trim();
        const invalid = Boolean(trimmed) && !IPV4.test(trimmed);
        return (
          <div className="ipv4-row" key={index}>
            <Input
              className={invalid ? "input has-error" : "input mono"}
              value={item}
              disabled={disabled}
              placeholder={placeholder}
              onChange={(event) => update(index, event.target.value)}
            />
            <Button
              size="sm"
              variant="ghost"
              icon={<Trash2 size={13} />}
              disabled={disabled || (rows.length === 1 && !item)}
              onClick={() => remove(index)}
            >
              删除
            </Button>
            {invalid && <span className="field-error">请输入 IPv4 地址，例如 203.0.113.10</span>}
          </div>
        );
      })}
      <div className="ipv4-actions">
        <Button size="sm" variant="secondary" icon={<Plus size={13} />} disabled={disabled} onClick={() => onChange([...rows, ""])}>
          新增地址
        </Button>
        <span className="field-hint">{hint || "按顺序尝试；留空使用 WAN 接口地址"}</span>
      </div>
    </div>
  );
}
