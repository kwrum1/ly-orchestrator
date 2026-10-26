import { useEffect, useMemo, useRef } from "react";
import * as echarts from "echarts/core";
import { LineChart } from "echarts/charts";
import { GridComponent, GraphicComponent, TooltipComponent } from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";
import type { LogicalEgressSeries } from "../api/types";
import { formatRate, formatTime } from "../lib/format";

echarts.use([LineChart, GridComponent, GraphicComponent, TooltipComponent, CanvasRenderer]);

const PALETTE = ["#2563eb", "#0ea5e9", "#10b981", "#f59e0b", "#8b5cf6", "#ef4444", "#14b8a6", "#64748b"];

export function seriesColor(index: number) {
  return PALETTE[index % PALETTE.length];
}

function valueAt(series: LogicalEgressSeries, timestamp: string, direction: "upload" | "download" | "total") {
  const sample = series.samples?.find((item) => item.timestamp === timestamp);
  if (!sample) return null;
  if (direction === "total") {
    return Number(sample.download_bps || 0) + Number(sample.upload_bps || 0);
  }
  return Number(sample[direction === "upload" ? "upload_bps" : "download_bps"] ?? 0);
}

export function TrafficChart({
  series,
  direction,
  height = 260,
  hidden = new Set<string>(),
}: {
  series: LogicalEgressSeries[];
  direction: "upload" | "download" | "total";
  height?: number;
  hidden?: Set<string>;
}) {
  const ref = useRef<HTMLDivElement>(null);
  const chartRef = useRef<echarts.ECharts | null>(null);

  const model = useMemo(() => {
    const visible = series.filter((item) => !hidden.has(item.id));
    const timestamps = [...new Set(visible.flatMap((item) => (item.samples || []).map((sample) => sample.timestamp)))].sort();
    const values = visible.map((item) => timestamps.map((timestamp) => valueAt(item, timestamp, direction)));
    const maximum = Math.max(0, ...values.flatMap((row) => row.filter((value): value is number => value !== null)));
    return { visible, timestamps, values, maximum };
  }, [series, direction, hidden]);

  useEffect(() => {
    if (!ref.current) return;
    chartRef.current = echarts.init(ref.current, undefined, { renderer: "canvas" });
    const observer = new ResizeObserver(() => chartRef.current?.resize());
    observer.observe(ref.current);
    return () => {
      observer.disconnect();
      chartRef.current?.dispose();
      chartRef.current = null;
    };
  }, []);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    const hasData = model.values.some((row) => row.some((value) => value !== null));
    chart.setOption({
      animation: false,
      grid: { left: 52, right: 12, top: 12, bottom: 28 },
      tooltip: {
        trigger: "axis",
        backgroundColor: "rgba(17, 24, 39, .92)",
        borderWidth: 0,
        textStyle: { color: "#fff", fontSize: 12 },
        formatter: (params: any[]) => {
          if (!params?.length) return "";
          const timestamp = model.timestamps[params[0].dataIndex];
          const rows = model.visible.map((item, index) => {
            const value = model.values[index][params[0].dataIndex];
            return `<div style="display:flex;justify-content:space-between;gap:18px"><span>${item.name || item.id}</span><b>${formatRate(value)}</b></div>`;
          }).join("");
          const total = model.values.reduce((sum, row) => sum + Number(row[params[0].dataIndex] || 0), 0);
          return `<div style="min-width:180px"><div style="margin-bottom:6px;color:#cbd5e1">${formatTime(timestamp)}</div>${rows}<div style="margin-top:6px;border-top:1px solid rgba(255,255,255,.15);padding-top:5px;display:flex;justify-content:space-between;gap:18px"><span>合计</span><b>${formatRate(total)}</b></div></div>`;
        },
      },
      xAxis: {
        type: "category",
        boundaryGap: false,
        data: model.timestamps,
        axisLine: { lineStyle: { color: "#dbe3ee" } },
        axisTick: { show: false },
        axisLabel: {
          color: "#8a97a8",
          fontSize: 11,
          formatter: (value: string) => formatTime(value),
          hideOverlap: true,
        },
      },
      yAxis: {
        type: "value",
        min: 0,
        splitNumber: 4,
        axisLabel: { color: "#8a97a8", fontSize: 11, formatter: (value: number) => formatRate(value) },
        splitLine: { lineStyle: { color: "#eef2f7", type: "dashed" } },
      },
      series: model.visible.map((item, index) => ({
        name: item.name || item.id,
        type: "line",
        showSymbol: false,
        smooth: 0.2,
        connectNulls: false,
        lineStyle: { width: 2, color: seriesColor(index) },
        itemStyle: { color: seriesColor(index) },
        areaStyle: {
          color: {
            type: "linear",
            x: 0,
            y: 0,
            x2: 0,
            y2: 1,
            colorStops: [
              { offset: 0, color: `${seriesColor(index)}26` },
              { offset: 1, color: `${seriesColor(index)}00` },
            ],
          },
        },
        data: model.values[index],
      })),
    }, { notMerge: true });
    if (!hasData) {
      chart.clear();
      chart.setOption({
        animation: false,
        graphic: [{ type: "text", left: "center", top: "middle", style: { text: "暂无流量记录", fill: "#8a97a8", fontSize: 12 } }],
      });
    }
  }, [model]);

  return <div ref={ref} className="traffic-chart" style={{ height }} role="img" aria-label="WAN 流量趋势图" />;
}

export function Sparkline({ values, color = "#2563eb", width = 78, height = 26 }: { values: number[]; color?: string; width?: number; height?: number }) {
  const points = values.filter((value) => Number.isFinite(value));
  if (points.length < 2) return <span className="spark-empty" style={{ width, height }} />;
  const max = Math.max(...points, 1);
  const min = Math.min(...points, 0);
  const span = max - min || 1;
  const path = points.map((value, index) => {
    const x = (index / (points.length - 1)) * width;
    const y = height - ((value - min) / span) * (height - 4) - 2;
    return `${index ? "L" : "M"}${x.toFixed(1)},${y.toFixed(1)}`;
  }).join(" ");
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} className="sparkline" aria-hidden="true">
      <path d={path} fill="none" stroke={color} strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

export function latestValue(item: LogicalEgressSeries, direction: "upload" | "download" | "total") {
  const sample = item.samples?.[item.samples.length - 1];
  if (!sample) return null;
  if (direction === "total") return Number(sample.download_bps || 0) + Number(sample.upload_bps || 0);
  return Number(sample[direction === "upload" ? "upload_bps" : "download_bps"] ?? 0);
}

export function totalLatest(series: LogicalEgressSeries[], direction: "upload" | "download") {
  let total = 0;
  let seen = false;
  for (const item of series) {
    const value = latestValue(item, direction);
    if (value !== null) {
      total += value;
      seen = true;
    }
  }
  return seen ? total : null;
}
