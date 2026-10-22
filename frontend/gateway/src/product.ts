export type Product = "gateway" | "orchestrator";

export const PRODUCT: Product = __LY_PRODUCT__ === "orchestrator" ? "orchestrator" : "gateway";
export const IS_ORCHESTRATOR = PRODUCT === "orchestrator";

export const PRODUCT_CONFIG = {
  gateway: {
    id: "gateway" as const,
    name: "Ly Route",
    subtitle: "出口网关",
    badge: "",
    loginTitle: "登录管理界面",
    loginSubtitle: "使用设备管理员账号访问路由、策略与运行态。",
    loginFooter: "Ly Route Gateway",
    documentTitle: "Ly Route 出口网关",
  },
  orchestrator: {
    id: "orchestrator" as const,
    name: "Ly Route",
    subtitle: "流量编排器",
    badge: "编排",
    loginTitle: "登录编排控制台",
    loginSubtitle: "单机 VPP 流量编排：本地拓扑、策略、服务链与遥测。",
    loginFooter: "Ly Route Orchestrator",
    documentTitle: "Ly Route 流量编排器",
  },
};

export const CURRENT_PRODUCT = PRODUCT_CONFIG[PRODUCT];
