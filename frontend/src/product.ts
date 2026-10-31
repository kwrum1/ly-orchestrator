// The orchestrator UI is a single-product application. The product switch that
// used to live here (and in ly-route's gateway UI) is gone: this repository
// builds the orchestrator console only.
export type Product = "orchestrator";

export const PRODUCT: Product = "orchestrator";
export const IS_ORCHESTRATOR = true;

export const PRODUCT_CONFIG = {
  orchestrator: {
    id: "orchestrator" as const,
    name: "Ly Route",
    subtitle: "流量编排器",
    badge: "编排",
    loginTitle: "登录编排控制台",
    loginSubtitle: "单机流量编排：本地拓扑、策略与服务链",
    loginFooter: "Ly Route Orchestrator",
    documentTitle: "Ly Route 流量编排器",
  },
};

export const CURRENT_PRODUCT = PRODUCT_CONFIG[PRODUCT];
