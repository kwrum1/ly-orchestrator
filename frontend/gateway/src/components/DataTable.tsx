import { useMemo, useState } from "react";
import {
  flexRender,
  getCoreRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
  type VisibilityState,
} from "@tanstack/react-table";
import { ArrowDown, ArrowUp, ChevronLeft, ChevronRight, Columns3, Inbox, RefreshCw } from "lucide-react";
import { Button, EmptyState, ErrorState, IconButton, Skeleton } from "./ui";
import { classNames } from "../lib/format";

export interface DataTableProps<T> {
  data: T[];
  columns: ColumnDef<T, any>[];
  loading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  emptyTitle?: string;
  emptyDescription?: string;
  emptyAction?: React.ReactNode;
  onRowClick?: (row: T) => void;
  getRowId?: (row: T, index: number) => string;
  initialPageSize?: number;
  pageSizeOptions?: number[];
  className?: string;
}

export function DataTable<T>({
  data,
  columns,
  loading,
  error,
  onRetry,
  emptyTitle = "暂无数据",
  emptyDescription,
  emptyAction,
  onRowClick,
  getRowId,
  initialPageSize = 20,
  pageSizeOptions = [20, 50, 100],
  className,
}: DataTableProps<T>) {
  const [sorting, setSorting] = useState<SortingState>([]);
  const [columnVisibility, setColumnVisibility] = useState<VisibilityState>({});
  const [showColumns, setShowColumns] = useState(false);

  const table = useReactTable({
    data,
    columns,
    state: { sorting, columnVisibility },
    onSortingChange: setSorting,
    onColumnVisibilityChange: setColumnVisibility,
    getCoreRowModel: getCoreRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    getRowId,
    initialState: { pagination: { pageSize: initialPageSize, pageIndex: 0 } },
  });

  const hideableColumns = useMemo(
    () => table.getAllLeafColumns().filter((column) => column.getCanHide() && (column.columnDef.meta as any)?.hideable !== false),
    [table]
  );

  if (error) return <ErrorState error={error} onRetry={onRetry} />;
  if (loading) return <Skeleton rows={6} />;

  const rows = table.getRowModel().rows;
  const total = data.length;

  return (
    <div className={classNames("data-table-wrap", className)}>
      <div className="table-tools">
        <div className="table-count">共 {total} 条{total !== data.length ? "" : ""}</div>
        <div className="table-tools-right">
          <div className="column-picker">
            <Button size="sm" variant="ghost" icon={<Columns3 size={14} />} onClick={() => setShowColumns((value) => !value)}>
              列设置
            </Button>
            {showColumns && (
              <div className="column-menu">
                {hideableColumns.map((column) => (
                  <label key={column.id}>
                    <input
                      type="checkbox"
                      checked={column.getIsVisible()}
                      onChange={column.getToggleVisibilityHandler()}
                    />
                    <span>{String(column.columnDef.header || column.id)}</span>
                  </label>
                ))}
              </div>
            )}
          </div>
          {onRetry && <IconButton label="刷新" onClick={onRetry}><RefreshCw size={15} /></IconButton>}
        </div>
      </div>
      <div className="table-scroll">
        <table className="data-table">
          <thead>
            {table.getHeaderGroups().map((headerGroup) => (
              <tr key={headerGroup.id}>
                {headerGroup.headers.map((header) => {
                  const meta = header.column.columnDef.meta as any;
                  return (
                    <th
                      key={header.id}
                      style={{ width: meta?.width, minWidth: meta?.minWidth }}
                      className={classNames(meta?.align && `align-${meta.align}`, meta?.sticky && "sticky-col")}
                    >
                      {header.isPlaceholder ? null : (
                        <button
                          type="button"
                          className={classNames("th-button", header.column.getCanSort() && "is-sortable")}
                          onClick={header.column.getToggleSortingHandler()}
                        >
                          {flexRender(header.column.columnDef.header, header.getContext())}
                          {header.column.getIsSorted() === "asc" && <ArrowUp size={12} />}
                          {header.column.getIsSorted() === "desc" && <ArrowDown size={12} />}
                        </button>
                      )}
                    </th>
                  );
                })}
              </tr>
            ))}
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr
                key={row.id}
                onClick={() => onRowClick?.(row.original)}
                className={classNames(onRowClick && "is-clickable")}
              >
                {row.getVisibleCells().map((cell) => {
                  const meta = cell.column.columnDef.meta as any;
                  return (
                    <td
                      key={cell.id}
                      className={classNames(meta?.align && `align-${meta.align}`, meta?.sticky && "sticky-col", meta?.mono && "mono")}
                    >
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  );
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {!rows.length && (
        <EmptyState
          title={emptyTitle}
          description={emptyDescription}
          action={emptyAction}
          icon={<Inbox size={22} />}
        />
      )}
      {total > 0 && (
        <div className="table-pagination">
          <div className="page-size">
            每页
            <select
              value={table.getState().pagination.pageSize}
              onChange={(event) => table.setPageSize(Number(event.target.value))}
            >
              {pageSizeOptions.map((size) => <option key={size} value={size}>{size}</option>)}
            </select>
            条
          </div>
          <div className="page-nav">
            <IconButton label="上一页" onClick={() => table.previousPage()} disabled={!table.getCanPreviousPage()}>
              <ChevronLeft size={15} />
            </IconButton>
            <span>{table.getState().pagination.pageIndex + 1} / {Math.max(1, table.getPageCount())}</span>
            <IconButton label="下一页" onClick={() => table.nextPage()} disabled={!table.getCanNextPage()}>
              <ChevronRight size={15} />
            </IconButton>
          </div>
        </div>
      )}
    </div>
  );
}
