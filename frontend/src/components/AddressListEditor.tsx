import { useState } from "react";
import { Search } from "lucide-react";
import { Button, Input, Modal } from "./ui";

type GroupRef = { id: string; name?: string; kind?: string; members?: string[] };

/**
 * Panabit-style IP group selector: dropdown + search button, opening a
 * single-select table dialog built on the product modal/table styles.
 */
export function AddressListEditor({
  value,
  onChange,
  groups = [],
  placeholder = "任意",
}: {
  value: string[];
  onChange: (value: string[]) => void;
  placeholder?: string;
  groups?: GroupRef[];
  rows?: number;
}) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [pending, setPending] = useState("");
  const [manualMode, setManualMode] = useState(false);

  const current = (value || [])[0] || "";
  const isGroup = groups.some((group) => group.id === current);
  const manual = manualMode || (current !== "" && !isGroup);

  const select = (next: string) => {
    if (next === "__manual__") {
      setManualMode(true);
      if (isGroup) onChange([""]);
      return;
    }
    setManualMode(false);
    onChange(next ? [next] : []);
  };

  const openPicker = () => {
    setQuery("");
    setPending(isGroup ? current : "");
    setOpen(true);
  };

  const confirm = () => {
    setManualMode(false);
    onChange(pending ? [pending] : []);
    setOpen(false);
  };

  const filtered = groups.filter((group) => {
    if (!query.trim()) return true;
    return `${group.name || ""} ${group.id}`.toLowerCase().includes(query.trim().toLowerCase());
  });

  return (
    <div className="address-field">
      <div className="address-control">
        <select
          className="select"
          value={isGroup ? current : manualMode ? "__manual__" : ""}
          onChange={(event) => select(event.target.value)}
        >
          <option value="">{placeholder}</option>
          {groups.map((group) => (
            <option key={group.id} value={group.id}>{group.name || group.id}</option>
          ))}
          <option value="__manual__">网络 / 前缀（手动输入）</option>
        </select>
        <button type="button" className="address-search-btn" title="选择 IP 群组" aria-label="选择 IP 群组" onClick={openPicker}>
          <Search size={15} />
        </button>
      </div>
      {manual && (
        <Input
          className="address-manual"
          value={current}
          onChange={(event) => onChange([event.target.value])}
          placeholder="x.x.x.x/y 或 x.x.x.x-x.x.x.x"
        />
      )}

      <Modal
        open={open}
        onClose={() => setOpen(false)}
        title="选择 IP 群组"
        size="md"
        footer={
          <>
            <Button onClick={() => setOpen(false)}>取消</Button>
            <Button variant="primary" onClick={confirm}>确定</Button>
          </>
        }
      >
        <div className="picker-search">
          <Search size={14} />
          <Input
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="搜索群组名称"
            autoFocus
          />
        </div>
        <div className="picker-table-wrap">
          <table className="picker-table">
            <thead>
              <tr>
                <th className="picker-check">选择</th>
                <th>名称</th>
                <th className="picker-count">成员</th>
              </tr>
            </thead>
            <tbody>
              {!filtered.length && (
                <tr>
                  <td colSpan={3}><div className="picker-empty">没有匹配的 IP 群组</div></td>
                </tr>
              )}
              {filtered.map((group) => {
                const checked = pending === group.id;
                return (
                  <tr
                    key={group.id}
                    className={checked ? "is-selected" : ""}
                    onClick={() => setPending(group.id)}
                    onDoubleClick={() => {
                      setManualMode(false);
                      onChange([group.id]);
                      setOpen(false);
                    }}
                  >
                    <td className="picker-check"><span className={checked ? "picker-radio is-on" : "picker-radio"} /></td>
                    <td><strong>{group.name || group.id}</strong></td>
                    <td className="picker-count">{(group.members || []).length || "—"}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </Modal>
    </div>
  );
}
