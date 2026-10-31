import { linkInfo } from "../lib/human";

/**
 * The link column always shows both rates:
 *   - grey: port capability (NIC/PMD maximum);
 *   - amber: negotiated rate, 接口 DOWN for a down link, "—" when unknown.
 */
export function LinkChips({ item }: { item: any }) {
  const link = linkInfo(item);
  return (
    <span className="link-pair" title={link.detail}>
      <span className="link-chip is-capability">{link.capability}</span>
      <span className={`link-chip is-actual${link.down ? " is-down" : ""}${link.pending ? " is-pending" : ""}`}>{link.actual}</span>
    </span>
  );
}
