import { useMemo, useState } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Eye, Filter, RefreshCw, Search, Users } from "lucide-react";
import { useOnlineUsers, useSessions } from "../api/hooks";
import type { OnlineUser, SessionItem } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, Drawer, EmptyState, Input, PageHeader, Select, Skeleton, Toolbar, CopyText } from "../components/ui";
import { formatBytes, formatDuration, formatRate, isOnlineUser, text, timeAgo } from "../lib/format";

function userIP(user: OnlineUser) {
  return String(user.ip || user.ip_address || user.address || user.user || "").trim();
}

function userDuration(user: OnlineUser) {
  const value = user.online_duration_seconds ?? user.online_duration ?? user.duration;
  return formatDuration(value);
}

function userTotalBytes(user: OnlineUser) {
  return Number(user.rx_bytes || 0) + Number(user.tx_bytes || 0);
}

function connectionCount(user: OnlineUser) {
  return Number(user.sessions || user.connections || user.connection_count || 0);
}

function sessionEndpoint(item: SessionItem, side: "source" | "destination") {
  const source = side === "source";
  const address = source
    ? item.src_ip || item.source_ip || item.src
    : item.translated_ip || item.dst_ip || item.destination_ip || item.dst;
  const port = source ? item.src_port || item.source_port : item.translated_port || item.dst_port || item.destination_port;
  return [address, port].filter((value) => value !== undefined && value !== null && value !== "").join(":") || "--";
}

function sessionState(item: SessionItem) {
  return String(item.session_kind || item.state || item.status || "活跃");
}

export function ClientsPage() {
  const online = useOnlineUsers();
  const sessions = useSessions();
  const [selected, setSelected] = useState<OnlineUser | null>(null);
  const [query, setQuery] = useState("");
  const [protocol, setProtocol] = useState("");
  const [stateFilter, setStateFilter] = useState("");
  const [page, setPage] = useState(1);

  const users = online.data?.data?.items || [];
  const filteredUsers = useMemo(() => {
    const value = query.trim().toLowerCase();
    if (!value) return users;
    return users.filter((user) => [userIP(user), user.mac, user.hostname, user.device_name].some((item) => String(item || "").toLowerCase().includes(value)));
  }, [users, query]);

  const selectedIP = selected ? userIP(selected) : "";
  const allSessions = sessions.data?.data?.items || [];
  const userSessions = useMemo(() => {
    if (!selectedIP) return [];
    return allSessions.filter((item) => {
      const source = String(item.src_ip || item.source_ip || item.src || "");
      return source === selectedIP || source.startsWith(`${selectedIP}:`);
    });
  }, [allSessions, selectedIP]);

  const protocols = useMemo(() => [...new Set(userSessions.map((item) => String(item.protocol || "").toUpperCase()).filter(Boolean))], [userSessions]);
  const states = useMemo(() => [...new Set(userSessions.map(sessionState).filter(Boolean))], [userSessions]);
  const filteredSessions = useMemo(() => {
    const value = query.trim().toLowerCase();
    return userSessions.filter((item) => {
      const protocolMatch = !protocol || String(item.protocol || "").toUpperCase() === protocol;
      const stateMatch = !stateFilter || sessionState(item) === stateFilter;
      const textMatch = !value || [sessionEndpoint(item, "source"), sessionEndpoint(item, "destination"), item.protocol, item.session_kind].some((candidate) => String(candidate || "").toLowerCase().includes(value));
      return protocolMatch && stateMatch && textMatch;
    });
  }, [userSessions, query, protocol, stateFilter]);
  const pageSize = 20;
  const totalPages = Math.max(1, Math.ceil(filteredSessions.length / pageSize));
  const pageRows = filteredSessions.slice((Math.min(page, totalPages) - 1) * pageSize, Math.min(page, totalPages) * pageSize);

  const columns = useMemo<ColumnDef<OnlineUser, any>[]>(() => [
    {
      id: "ip",
      header: "IP / 设备",
      accessorFn: (row) => userIP(row),
      cell: ({ row }) => {
        const user = row.original;
        return (
          <button type="button" className="link-button" onClick={() => { setSelected(user); setPage(1); setQuery(""); setProtocol(""); setStateFilter(""); }}>
            <strong>{text(userIP(user))}</strong>
            <small>{text(user.hostname || user.device_name, "未命名设备")}</small>
          </button>
        );
      },
      meta: { width: 220 },
    },
    { id: "mac", header: "MAC", accessorFn: (row) => row.mac || row.mac_address, cell: ({ getValue }) => <span className="mono">{text(getValue())}</span>, meta: { width: 150, mono: true } },
    {
      id: "status",
      header: "状态",
      accessorFn: (row) => isOnlineUser(row),
      cell: ({ row }) => <Badge tone={isOnlineUser(row.original) ? "success" : "neutral"} dot>{isOnlineUser(row.original) ? "在线" : "离线"}</Badge>,
      meta: { width: 90 },
    },
    { id: "sessions", header: "连接数", accessorFn: (row) => connectionCount(row), cell: ({ getValue }) => Number(getValue()).toLocaleString("zh-CN"), meta: { width: 86, align: "right" } },
    { id: "up", header: "当前上行", accessorFn: (row) => row.tx_bps || 0, cell: ({ getValue }) => formatRate(getValue()), meta: { width: 112, align: "right" } },
    { id: "down", header: "当前下行", accessorFn: (row) => row.rx_bps || 0, cell: ({ getValue }) => formatRate(getValue()), meta: { width: 112, align: "right" } },
    { id: "duration", header: "在线时长", accessorFn: (row) => row.online_duration_seconds || 0, cell: ({ row }) => userDuration(row.original), meta: { width: 112 } },
    { id: "total", header: "总流量", accessorFn: (row) => userTotalBytes(row), cell: ({ getValue }) => formatBytes(getValue()), meta: { width: 104, align: "right" } },
    {
      id: "actions",
      header: "操作",
      cell: ({ row }) => <Button size="sm" variant="text" icon={<Eye size={14} />} onClick={() => { setSelected(row.original); setPage(1); }}>详情</Button>,
      meta: { width: 84, align: "right", hideable: false },
    },
  ], []);

  return (
    <div className="page">
      <PageHeader
        title="在线用户"
        subtitle="DHCP 租约与邻居表合成的在线终端；点击 IP 查看会话"
        actions={<Button size="sm" icon={<RefreshCw size={14} />} onClick={() => { online.refetch(); sessions.refetch(); }}>刷新</Button>}
      />
      <Toolbar
        left={
          <>
            <label className="search-field"><Search size={14} /><Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索 IP、MAC 或主机名" /></label>
            <Badge tone="neutral">{filteredUsers.length} 个用户</Badge>
          </>
        }
        right={<span className="muted"><Users size={14} /> 数据来自 DHCP 租约 + 邻居</span>}
      />
      <DataTable
        data={filteredUsers}
        columns={columns}
        loading={online.isLoading}
        error={online.isError ? online.error : null}
        onRetry={() => online.refetch()}
        emptyTitle="暂无在线用户"
        emptyDescription="DHCP 租约或邻居表为空；确认 LAN/DHCP 与遥测已配置。"
        getRowId={(row, index) => `${userIP(row)}-${index}`}
      />

      <Drawer
        open={Boolean(selected)}
        onClose={() => setSelected(null)}
        title={selected ? `${userIP(selected)} 会话` : "用户会话"}
        subtitle={selected ? text(selected.hostname || selected.device_name, "未命名设备") : ""}
        width="lg"
        footer={<Button onClick={() => setSelected(null)}>关闭</Button>}
      >
        {selected && (
          <>
            <div className="drawer-summary">
              <div><span>MAC 地址</span><strong className="mono">{text(selected.mac || selected.mac_address)}</strong></div>
              <div><span>状态</span><strong><Badge tone={isOnlineUser(selected) ? "success" : "neutral"} dot>{isOnlineUser(selected) ? "在线" : "离线"}</Badge></strong></div>
              <div><span>在线时长</span><strong>{userDuration(selected)}</strong></div>
              <div><span>当前速率</span><strong>{formatRate(Number(selected.rx_bps || 0) + Number(selected.tx_bps || 0))}</strong></div>
              <div><span>累计流量</span><strong>{formatBytes(userTotalBytes(selected))}</strong></div>
              <div><span>最后活跃</span><strong>{timeAgo(selected.last_traffic_time)}</strong></div>
            </div>
            <div className="session-filters">
              <label className="search-field"><Search size={14} /><Input value={query} onChange={(event) => { setQuery(event.target.value); setPage(1); }} placeholder="源/目的/NAT 地址" /></label>
              <Select value={protocol} onChange={(event) => { setProtocol(event.target.value); setPage(1); }}>
                <option value="">全部协议</option>
                {protocols.map((item) => <option key={item} value={item}>{item}</option>)}
              </Select>
              <Select value={stateFilter} onChange={(event) => { setStateFilter(event.target.value); setPage(1); }}>
                <option value="">全部状态</option>
                {states.map((item) => <option key={item} value={item}>{item}</option>)}
              </Select>
              <Button size="sm" variant="ghost" icon={<Filter size={14} />} onClick={() => { setQuery(""); setProtocol(""); setStateFilter(""); setPage(1); }}>清空</Button>
            </div>
            <div className="session-table-head">
              <strong>会话明细</strong>
              <span className="muted">{filteredSessions.length} 条</span>
            </div>
            {sessions.isLoading ? <Skeleton rows={6} /> : (
              <div className="session-table-wrap">
                <table className="data-table session-table">
                  <thead><tr><th>源地址:端口</th><th>目的 / NAT</th><th>协议</th><th>状态 / 时长</th><th className="align-right">上行/下行</th></tr></thead>
                  <tbody>
                    {pageRows.map((item, index) => (
                      <tr key={index}>
                        <td className="mono">{sessionEndpoint(item, "source")}</td>
                        <td className="mono">{sessionEndpoint(item, "destination")}</td>
                        <td><Badge tone="info">{String(item.protocol || "—").toUpperCase()}</Badge></td>
                        <td>{sessionState(item)}<small className="muted"> · {formatDuration(item.age_seconds)}</small></td>
                        <td className="align-right">{formatBytes(item.bytes)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
                {!pageRows.length && <EmptyState title="暂无符合条件的会话" description="调整筛选条件或刷新会话。" icon={<Filter size={22} />} />}
              </div>
            )}
            <div className="drawer-pager">
              <Button size="sm" variant="ghost" disabled={page <= 1} onClick={() => setPage((value) => Math.max(1, value - 1))}>上一页</Button>
              <span>{Math.min(page, totalPages)} / {totalPages}</span>
              <Button size="sm" variant="ghost" disabled={page >= totalPages} onClick={() => setPage((value) => Math.min(totalPages, value + 1))}>下一页</Button>
            </div>
            <p className="drawer-note">会话字段来自控制面遥测；后端不返回策略/出口归属时，页面不展示推断结果。</p>
          </>
        )}
      </Drawer>
    </div>
  );
}
