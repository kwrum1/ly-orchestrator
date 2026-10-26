import { useMemo } from "react";
import { Textarea } from "./ui";
import { splitLines } from "../lib/format";

export function AddressListEditor({
  value,
  onChange,
  placeholder = "每行一个地址或网段；留空表示任意",
  groups = [],
  rows = 4,
}: {
  value: string[];
  onChange: (value: string[]) => void;
  placeholder?: string;
  groups?: { id: string; name?: string; kind?: string }[];
  rows?: number;
}) {
  const text = useMemo(() => (value || []).join("\n"), [value]);
  return (
    <div className="address-editor">
      <Textarea
        rows={rows}
        value={text}
        placeholder={placeholder}
        onChange={(event) => onChange(splitLines(event.target.value))}
      />
      {groups.length > 0 && (
        <div className="group-chips">
          <span>插入组：</span>
          {groups.slice(0, 12).map((group) => (
            <button
              type="button"
              className="chip chip-button"
              key={group.id}
              onClick={() => onChange([...(value || []), group.id])}
            >
              {group.name || group.id}
            </button>
          ))}
        </div>
      )}
      <small className="field-hint">空列表 = 任意；支持 CIDR、范围与 IP 组 ID。</small>
    </div>
  );
}
