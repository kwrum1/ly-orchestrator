// Proxy egress status wording.
//
// The egress list is configuration; availability is node-based. An egress binds
// a node group (or a single node/subscription), so the operator-facing state is
// "N 节点可用" / "无节点可用" / "未绑定节点组". When the whole proxy feature has
// no egress, the single product wording is "当前无代理出口".

export const NO_PROXY_EGRESS = "当前无代理出口";

export interface ProxyEgressStatus {
  label: string;
  tone: "success" | "warning" | "neutral";
  detail: string;
}

export interface ProxyStatusContext {
  groups?: any[];
  nodes?: any[];
  subscriptions?: any[];
  status?: any;
}

function nodeIsDead(status: any, nodeID: string): boolean {
  const dead = Array.isArray(status?.dead_node_ids) ? status.dead_node_ids.map(String) : [];
  return dead.includes(String(nodeID));
}

function subscriptionNodeIDs(subscriptions: any[], nodes: any[], subscriptionID: string): string[] {
  const subscription = (subscriptions || []).find((item) => String(item?.id) === subscriptionID);
  const refs = Array.isArray(subscription?.node_refs) ? subscription.node_refs.map(String) : [];
  if (refs.length) return refs;
  return (nodes || []).filter((item) => String(item?.subscription_id || "") === subscriptionID).map((item) => String(item.id));
}

/**
 * proxyEgressStatus maps one proxy egress to its node-based state.
 *
 * `status` is the /proxy/xray/status payload (all_nodes_down, dead_node_ids,
 * balancers). A missing status is not a failure: unknown nodes stay "可用"
 * until the runtime reports them dead.
 */
export function proxyEgressStatus(egress: any, context: ProxyStatusContext = {}): ProxyEgressStatus {
  const status = context.status || {};
  const groups = context.groups || [];
  const nodes = context.nodes || [];
  const subscriptions = context.subscriptions || [];

  const groupID = String(egress?.group_id || "").trim();
  const nodeID = String(egress?.node_id || "").trim();
  const subscriptionID = String(egress?.subscription_id || "").trim();

  let memberIDs: string[] = [];
  let groupBound = false;
  if (groupID) {
    groupBound = true;
    const group = groups.find((item) => String(item?.id) === groupID);
    const members = Array.isArray(group?.members) ? group.members.map(String).filter(Boolean) : [];
    const balancer = (status?.balancers || []).find((item: any) => String(item?.group_id || "") === groupID);
    memberIDs = members.length ? members : Array.isArray(balancer?.member_node_ids) ? balancer.member_node_ids.map(String) : [];
  } else if (nodeID) {
    memberIDs = [nodeID];
  } else if (subscriptionID) {
    memberIDs = subscriptionNodeIDs(subscriptions, nodes, subscriptionID);
    if (!memberIDs.length) {
      return { label: "无节点可用", tone: "warning", detail: "订阅没有展开出可用节点。" };
    }
  }

  if (!memberIDs.length) {
    if (groupBound) {
      return { label: "未绑定节点组", tone: "warning", detail: "代理出口引用的节点组不存在或没有成员节点。" };
    }
    return { label: "未绑定节点组", tone: "warning", detail: "该代理出口还没有绑定节点或节点组。" };
  }

  if (status?.all_nodes_down) {
    return { label: "无节点可用", tone: "warning", detail: "所有代理节点健康检查失败。" };
  }
  const enabledNodes = new Map<string, boolean>();
  for (const node of nodes) enabledNodes.set(String(node?.id), node?.enabled !== false);
  const available = memberIDs.filter((id) => enabledNodes.get(String(id)) !== false && !nodeIsDead(status, id));
  if (available.length === 0) {
    return { label: "无节点可用", tone: "warning", detail: "节点组中的节点均不可用。" };
  }
  return {
    label: `${available.length} 节点可用`,
    tone: "success",
    detail: `${available.length}/${memberIDs.length} 个节点通过可用性检查。`,
  };
}
