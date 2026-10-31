import type { ReactNode } from "react";
import {
  Activity,
  ArrowLeftRight,
  Cable,
  ChartNoAxesCombined,
  FileJson,
  GitBranch,
  LayoutDashboard,
  Network,
  RefreshCcw,
  ServerCog,
  Settings2,
  ShieldCheck,
  ShieldAlert,
  Users,
  Waypoints,
  Workflow,
} from "lucide-react";
import type { NavGroup, NavPage } from "./nav";

export type { NavGroup, NavPage };

export const ORCHESTRATOR_NAV_GROUPS: NavGroup[] = [
  {
    key: "overview",
    label: "概览",
    pages: [
      { key: "orch-telemetry", path: "/", label: "流量监控", icon: <ChartNoAxesCombined size={16} />, description: "边界流量、策略命中与连接" },
    ],
  },
  {
    key: "topology",
    label: "拓扑与编排",
    pages: [
      { key: "orch-topology", path: "/topology", label: "网卡设置", icon: <Network size={16} />, description: "管理口、LAN/WAN、数据口与编排组" },
      { key: "orch-mirror", path: "/mirror", label: "镜像口", icon: <Cable size={16} />, description: "物理口镜像到专用抓包口" },
      { key: "orch-groups", path: "/groups", label: "编排组", icon: <Waypoints size={16} />, description: "LAN/WAN 成对服务链路径" },
      { key: "orch-bypass", path: "/bypass", label: "故障旁路", icon: <ShieldAlert size={16} />, description: "按组检测故障并仅旁路该组策略" },
      { key: "orch-policy", path: "/policy", label: "流量编排", icon: <GitBranch size={16} />, description: "IP 对象、策略组、规则与默认动作" },
    ],
  },
  {
    key: "system",
    label: "系统",
    pages: [
      { key: "orch-config", path: "/config", label: "系统配置", icon: <Settings2 size={16} />, description: "管理口、导出、导入、快照与恢复" },
      { key: "orch-upgrades", path: "/upgrades", label: "升级 / 回滚", icon: <RefreshCcw size={16} />, description: "编排器升级任务与回滚" },
      { key: "orch-users", path: "/system/users", label: "用户管理", icon: <Users size={16} />, description: "管理员与只读账号" },
    ],
  },
];

export const ORCHESTRATOR_ALL_PAGES = ORCHESTRATOR_NAV_GROUPS.flatMap((group) => group.pages);

export const ORCHESTRATOR_COMMAND_ACTIONS = [
  { key: "apply", label: "应用配置", description: "提交当前拓扑/策略并执行应用计划", icon: <ServerCog size={16} /> },
  { key: "refresh", label: "刷新当前页面", description: "重新读取当前页数据", icon: <RefreshCcw size={16} /> },
];

export const ORCHESTRATOR_LEGACY_ROUTES: Record<string, string> = {
  "dashboard/overview": "/",
  "telemetry/flow-summary": "/telemetry",
  "telemetry/online-users": "/telemetry",
  "telemetry/top-connections": "/telemetry",
  "orchestrator/nic-settings": "/topology",
  "orchestrator/port-mirror": "/mirror",
  "orchestrator/group-settings": "/groups",
  "orchestrator/policy": "/policy",
  "flow-control/traffic": "/telemetry",
  "security/policies": "/runtime",
  "object/ip": "/policy",
  "system/users": "/system/users",
  "system/config": "/config",
};

export const ORCHESTRATOR_EXTRA_ICONS = { ShieldCheck, FileJson, Activity };
