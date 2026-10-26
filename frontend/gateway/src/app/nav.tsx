import type { ReactNode } from "react";
import {
  Activity,
  Boxes,
  Cable,
  CircleGauge,
  CloudCog,
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
      { key: "overview", path: "/", label: "总览", icon: <LayoutDashboard size={16} />, description: "设备、流量与在线用户" },
    ],
  },
  {
    key: "monitor",
    label: "监控",
    pages: [
      { key: "clients", path: "/clients", label: "在线用户", icon: <Users size={16} />, description: "在线终端、会话与筛选" },
      { key: "connections", path: "/connections", label: "Top 连接", icon: <ListFilter size={16} />, description: "按会话数排序的连接" },
      { key: "domains", path: "/domains", label: "Top 域名", icon: <Globe2 size={16} />, description: "按查询次数排序的域名" },
    ],
  },
  {
    key: "network",
    label: "网络",
    pages: [
      { key: "interfaces", path: "/interfaces", label: "网卡设置", icon: <Cable size={16} />, description: "接口角色与链路状态" },
      { key: "wan", path: "/wan", label: "WAN 线路", icon: <Router size={16} />, description: "WAN、PPPoE 与代理 WAN" },
      { key: "wan-groups", path: "/wan-groups", label: "WAN 群组", icon: <Waypoints size={16} />, description: "主备与负载模式" },
      { key: "lan", path: "/lan", label: "LAN 线路", icon: <Cable size={16} />, description: "IPv4、IPv6、RA 与 DHCPv6" },
      { key: "dhcp", path: "/dhcp", label: "DHCP 服务", icon: <Wifi size={16} />, description: "地址池、静态绑定与租约" },
      { key: "proxy", path: "/proxy", label: "代理资源", icon: <CloudCog size={16} />, description: "节点、订阅、日志与故障页" },
    ],
  },
  {
    key: "policy",
    label: "策略",
    pages: [
      { key: "routing", path: "/routing", label: "策略路由", icon: <Waypoints size={16} />, description: "优先级、Any→Any 与出口" },
      { key: "port-mapping", path: "/port-mapping", label: "端口映射", icon: <Network size={16} />, description: "NAT 入站与静态映射" },
      { key: "dns", path: "/dns", label: "DNS 策略", icon: <Globe2 size={16} />, description: "域名、上游与解析线路" },
      { key: "qos", path: "/qos", label: "流控", icon: <CircleGauge size={16} />, description: "限速与阻断" },
      { key: "objects", path: "/objects", label: "对象管理", icon: <Boxes size={16} />, description: "域名组与 IP 组" },
    ],
  },
  {
    key: "system",
    label: "系统",
    pages: [
      { key: "config", path: "/system/config", label: "配置管理", icon: <Settings2 size={16} />, description: "管理口、时间与备份" },
      { key: "firmware", path: "/system/firmware", label: "固件升级", icon: <HardDrive size={16} />, description: "上传、校验与安装" },
      { key: "account", path: "/system/account", label: "管理员账号", icon: <Users size={16} />, description: "管理员密码与会话" },
      { key: "diagnostics", path: "/system/diagnostics", label: "网络诊断", icon: <Activity size={16} />, description: "单次 Ping" },
    ],
  },
];

export const ALL_PAGES = NAV_GROUPS.flatMap((group) => group.pages);

export const COMMAND_ACTIONS = [
  { key: "apply", label: "应用配置", description: "提交当前配置到数据面", icon: <ServerCog size={16} /> },
  { key: "refresh", label: "刷新当前页面", description: "重新读取当前页数据", icon: <SlidersHorizontal size={16} /> },
];

export const LEGACY_ROUTES: Record<string, string> = {
  "system/system_overview": "/",
  "dashboard/dashboard": "/",
  "monitor/ipobj_main": "/clients",
  "monitor/flow_topn": "/connections",
  "monitor/domain_topn": "/domains",
  "monitor/interface_list": "/interfaces",
  "network/proxy_main": "/proxy",
  "network/wangroup_manager": "/wan-groups",
  "route/route_policy_main": "/routing",
  "route/portmap_list": "/port-mapping",
  "route/dnspolicy_main": "/dns",
  "network/dhcpsvr_main": "/dhcp",
  "flowcontrol/flowct_main": "/qos",
  "object/urlgrp_list": "/objects",
  "object/iptab_list": "/objects",
  "system/webuser_main": "/system/account",
  "system/sys_config": "/system/config",
};

export const EXTRA_ICONS = { FileStack, ScrollText, Settings2 };
