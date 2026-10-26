import type { ReactNode } from "react";
import {
  Activity,
  ArrowLeftRight,
  ChartNoAxesCombined,
  FileJson,
  GitBranch,
  LayoutDashboard,
  Network,
  RefreshCcw,
  ServerCog,
  Settings2,
  ShieldCheck,
  Users,
  Waypoints,
  Workflow,
} from "lucide-react";
import type { NavGroup, NavPage } from "./nav";

export const ORCHESTRATOR_NAV_GROUPS: NavGroup[] = [
  {
    key: "overview",
    label: "概览",
    pages: [
      { key: "orch-overview", path: "/", label: "单机总览", icon: <LayoutDashboard size={16} />, description: "本机拓扑、策略与边界" },
    ],
  },
  {
    key: "topology",
    label: "拓扑与编排",
    pages: [
      { key: "orch-topology", path: "/topology", label: "单机拓扑", icon: <Network size={16} />, description: "管理口、LAN/WAN、数据口与编排组" },
      { key: "orch-groups", path: "/groups", label: "编排组", icon: <Waypoints size={16} />, description: "LAN/WAN 成对服务链路径" },
      { key: "orch-policy", path: "/policy", label: "流量编排", icon: <GitBranch size={16} />, description: "IP 对象、策略组、规则与默认动作" },
      { key: "orch-transparent", path: "/transparent", label: "透明编排", icon: <Workflow size={16} />, description: "服务链编译、应用与当前操作" },
    ],
  },
  {
    key: "observability",
    label: "运行与观测",
    pages: [
      { key: "orch-telemetry", path: "/telemetry", label: "流量监控", icon: <ChartNoAxesCombined size={16} />, description: "边界流量、策略命中与连接" },
      { key: "orch-runtime", path: "/runtime", label: "系统状态", icon: <Activity size={16} />, description: "组件、预览、应用与最近记录" },
    ],
  },
  {
    key: "system",
    label: "系统",
    pages: [
      { key: "orch-config", path: "/config", label: "配置与快照", icon: <Settings2 size={16} />, description: "管理口、导出、导入、快照与恢复" },
      { key: "orch-upgrades", path: "/upgrades", label: "升级 / 回滚", icon: <RefreshCcw size={16} />, description: "Orchestrator 升级任务与回滚" },
      { key: "orch-conversions", path: "/conversions", label: "产品转换", icon: <ArrowLeftRight size={16} />, description: "Gateway ↔ Orchestrator 校验、准备、501 边界" },
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
  "orchestrator/group-settings": "/groups",
  "orchestrator/policy": "/policy",
  "flow-control/traffic": "/telemetry",
  "security/policies": "/runtime",
  "object/ip": "/policy",
  "system/users": "/system/users",
  "system/config": "/config",
};

export const ORCHESTRATOR_EXTRA_ICONS = { ShieldCheck, FileJson, Activity };
