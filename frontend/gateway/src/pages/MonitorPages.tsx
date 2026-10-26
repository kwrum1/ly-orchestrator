import { useMemo } from "react";
import type { ColumnDef } from "@tanstack/react-table";
import { Globe2, ListFilter, RefreshCw } from "lucide-react";
import { useTopDomains, useTopSessions } from "../api/hooks";
import type { SessionItem } from "../api/types";
import { DataTable } from "../components/DataTable";
import { Badge, Button, PageHeader } from "../components/ui";
import { formatBytes, formatDateTime, text, timeAgo } from "../lib/format";

export function ConnectionsPage() {
  const sessions = useTopSessions();
  const rows = sessions.data?.data?.items || [];
  const columns = useMemo<ColumnDef<SessionItem, any>[]>(() => [
    { id: "source", header: "源地址", accessorFn: (row) => row.src_ip || row.source_ip || row.src, cell: ({ row }) => <span className="mono">{text(row.original.src_ip || row.original.source_ip || row.original.src)}</span>, meta: { width: 170, mono: true } },
    { id: "destination", header: "目的地址", accessorFn: (row) => row.dst_ip || row.destination_ip || row.dst, cell: ({ row }) => <span className="mono">{text(row.original.dst_ip || row.original.destination_ip || row.original.dst)}</span>, meta: { width: 170, mono: true } },
    { id: "protocol", header: "协议", accessorFn: (row) => row.protocol, cell: ({ getValue }) => <Badge tone="info">{String(getValue() || "—").toUpperCase()}</Badge>, meta: { width: 80 } },
    { id: "count", header: "连接数", accessorFn: (row) => row.connection_count || row.connections || 1, cell: ({ getValue }) => Number(getValue()).toLocaleString("zh-CN"), meta: { width: 90, align: "right" } },
    { id: "bytes", header: "流量", accessorFn: (row) => row.bytes || 0, cell: ({ getValue }) => formatBytes(getValue()), meta: { width: 110, align: "right" } },
    { id: "age", header: "时长", accessorFn: (row) => row.age_seconds || 0, cell: ({ getValue }) => `${Math.round(Number(getValue()) || 0)}s`, meta: { width: 80, align: "right" } },
    { id: "observed", header: "采样时间", accessorFn: (row) => row.observed_at, cell: ({ getValue }) => <span className="muted">{timeAgo(getValue())}</span>, meta: { width: 110 } },
  ], []);

  return (
    <div className="page">
      <PageHeader
        title="Top 连接"
        subtitle="按会话数排序的连接"
        actions={<Button size="sm" icon={<RefreshCw size={14} />} onClick={() => sessions.refetch()}>刷新</Button>}
      />
      <DataTable
        data={rows}
        columns={columns}
        loading={sessions.isLoading}
        error={sessions.isError ? sessions.error : null}
        onRetry={() => sessions.refetch()}
        emptyTitle="暂无连接数据"
        emptyDescription="暂无数据"
        getRowId={(row, index) => `${row.src_ip}-${row.dst_ip}-${index}`}
      />
    </div>
  );
}

export function DomainsPage() {
  const domains = useTopDomains();
  const rows = domains.data?.data?.items || [];
  const columns = useMemo<ColumnDef<any, any>[]>(() => [
    { id: "domain", header: "域名", accessorFn: (row) => row.domain || row.name, cell: ({ row }) => <span className="mono">{text(row.original.domain || row.original.name)}</span>, meta: { width: 360, mono: true } },
    { id: "last", header: "最后访问", accessorFn: (row) => row.last_seen || row.last_access || row.last_access_time, cell: ({ getValue }) => <span className="muted">{timeAgo(getValue())}</span>, meta: { width: 150 } },
    { id: "hits", header: "命中次数", accessorFn: (row) => row.hits || row.queries || row.count || 0, cell: ({ getValue }) => Number(getValue()).toLocaleString("zh-CN"), meta: { width: 110, align: "right" } },
  ], []);

  return (
    <div className="page">
      <PageHeader
        title="Top 域名"
        subtitle="按查询次数排序的域名"
        actions={<Button size="sm" icon={<RefreshCw size={14} />} onClick={() => domains.refetch()}>刷新</Button>}
      />
      <DataTable
        data={rows}
        columns={columns}
        loading={domains.isLoading}
        error={domains.isError ? domains.error : null}
        onRetry={() => domains.refetch()}
        emptyTitle="暂无域名数据"
        emptyDescription="暂无数据"
        getRowId={(row, index) => `${row.domain || row.name || index}`}
      />
    </div>
  );
}
