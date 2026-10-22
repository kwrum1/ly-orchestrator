import type { ReactNode } from "react";
import {
  Activity,
  Boxes,
  Cable,
  ChartNoAxesCombined,
  CircleGauge,
  CloudCog,
  Container,
  FileStack,
  Globe2,
  HardDrive,
  LayoutDashboard,
  ListFilter,
  Network,
  Router,
  ScrollText,
  ServerCog,
  Settings2,
  ShieldCheck,
  SlidersHorizontal,
  Users,
  Waypoints,
  Wifi,
} from "lucide-react";

export interface NavPage {
  key: string;
  path: string;
  label: string;
  icon: ReactNode;
  description: string;
}

export interface NavGroup {
  key: string;
  label: string;
  pages: NavPage[];
}

export const NAV_GROUPS: NavGroup[] = [
  {
    key: "overview",
    label: "概览",
    pages: [
      { key: "overview", path: "/", label: "总览", icon: <LayoutDashboard size={16} />, description: "设备健康、流量、出口与待处理" },
    ],
  },
  {
    key: "monitor",
    label: "监控",
    pages: [
      { key: "traffic", path: "/traffic", label: "流量概况", icon: <ChartNoAxesCombined size={16} />, description: "上下行趋势、出口与时间窗" },
      { key: "clients", path: "/clients", label: "在线用户", icon: <Users size={16} />, description: "在线终端、会话与筛选" },
      { key: "connections", path: "/connections", label: "连接排行", icon: <ListFilter size={16} />, description: "Top 连接" },
      { key: "domains", path: "/domains", label: "域名排行", icon: <Globe2 size={16} />, description: "Top 域名" },
    ],
  },
  {
    key: "network",
    label: "网络",
    pages: [
      { key: "interfaces", path: "/interfaces", label: "网卡设置", icon: <Cable size={16} />, description: "接口角色、链路与聚合" },
      { key: "wan", path: "/wan", label: "LAN/WAN", icon: <Router size={16} />, description: "LAN、WAN、PPPoE 与线路日志" },
      { key: "wan-groups", path: "/wan-groups", label: "WAN 群组", icon: <Waypoints size={16} />, description: "主备、权重与负载模式" },
      { key: "dhcp", path: "/dhcp", label: "DHCP 服务", icon: <Wifi size={16} />, description: "服务与静态分配" },
      { key: "proxy", path: "/proxy", label: "代理线路", icon: <CloudCog size={16} />, description: "Xray 出口、节点与订阅" },
    ],
  },
  {
    key: "policy",
    label: "策略与对象",
    pages: [
      { key: "routing", path: "/routing", label: "策略路由", icon: <Waypoints size={16} />, description: "优先级、Any→Any 与出口" },
      { key: "port-mapping", path: "/port-mapping", label: "端口映射", icon: <Network size={16} />, description: "NAT 入站与静态映射" },
      { key: "dns", path: "/dns", label: "DNS 策略", icon: <Globe2 size={16} />, description: "域名、上游与解析线路" },
      { key: "qos", path: "/qos", label: "流量控制", icon: <CircleGauge size={16} />, description: "限速、阻断与 Smart QoS" },
      { key: "objects-domains", path: "/objects/domains", label: "域名管理", icon: <Container size={16} />, description: "域名组与成员" },
      { key: "objects-ips", path: "/objects/ips", label: "IP 管理", icon: <Boxes size={16} />, description: "IP 组与成员" },
      { key: "security", path: "/security", label: "安全策略", icon: <ShieldCheck size={16} />, description: "ACL、IP-MAC、威胁与攻击规则" },
    ],
  },
  {
    key: "system",
    label: "系统",
    pages: [
      { key: "runtime", path: "/system/runtime", label: "运行态", icon: <Activity size={16} />, description: "组件、预览与应用" },
      { key: "config", path: "/system/config", label: "配置管理", icon: <Settings2 size={16} />, description: "管理口、初始化与时间" },
      { key: "firmware", path: "/system/firmware", label: "固件升级", icon: <HardDrive size={16} />, description: "上传、校验与安装" },
      { key: "users", path: "/system/users", label: "用户管理", icon: <Users size={16} />, description: "管理员与只读账号" },
    ],
  },
];

export const ALL_PAGES = NAV_GROUPS.flatMap((group) => group.pages);

export const COMMAND_ACTIONS = [
  { key: "apply", label: "应用运行态", description: "提交当前 desired state 到数据面", icon: <ServerCog size={16} /> },
  { key: "refresh", label: "刷新当前页面", description: "重新读取当前页数据", icon: <SlidersHorizontal size={16} /> },
];

export const LEGACY_ROUTES: Record<string, string> = {
  "system/system_overview": "/",
  "dashboard/dashboard": "/traffic",
  "monitor/ipobj_main": "/clients",
  "monitor/flow_topn": "/connections",
  "monitor/domain_topn": "/domains",
  "monitor/interface_list": "/interfaces",
  "network/proxy_main": "/wan",
  "network/wangroup_manager": "/wan-groups",
  "route/route_policy_main": "/routing",
  "route/portmap_list": "/port-mapping",
  "route/dnspolicy_main": "/dns",
  "network/dhcpsvr_main": "/dhcp",
  "flowcontrol/flowct_main": "/qos",
  "object/urlgrp_list": "/objects/domains",
  "object/iptab_list": "/objects/ips",
  "system/webuser_main": "/system/users",
  "system/sys_config": "/system/runtime",
};

export const EXTRA_ICONS = { FileStack, ScrollText, Settings2 };
