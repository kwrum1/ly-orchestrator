import type { ReactNode } from "react";

// Navigation types only. The concrete orchestrator navigation lives in
// orchestrator-nav.tsx; the gateway navigation stayed in ly-route.
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
