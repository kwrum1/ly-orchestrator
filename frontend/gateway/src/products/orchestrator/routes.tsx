import { Route, Routes } from "react-router-dom";
import { AppShell } from "../../app/AppShell";
import { ORCHESTRATOR_ALL_PAGES, ORCHESTRATOR_COMMAND_ACTIONS, ORCHESTRATOR_NAV_GROUPS } from "../../app/orchestrator-nav";
import { NotFoundPage } from "../../pages/NotFoundPage";
import { OrchestratorOverviewPage } from "../../pages/orchestrator/OverviewPage";
import { OrchestratorTopologyPage } from "../../pages/orchestrator/TopologyPage";
import { OrchestratorGroupsPage } from "../../pages/orchestrator/GroupsPage";
import { OrchestratorPolicyPage } from "../../pages/orchestrator/PolicyPage";
import { OrchestratorTransparentPage } from "../../pages/orchestrator/TransparentPage";
import { OrchestratorTelemetryPage } from "../../pages/orchestrator/TelemetryPage";
import { OrchestratorUpgradesPage } from "../../pages/orchestrator/UpgradesPage";
import { OrchestratorConversionsPage } from "../../pages/orchestrator/ConversionsPage";
import { RuntimePage } from "../../pages/RuntimePage";
import { ConfigPage } from "../../pages/ConfigPage";
import { UsersPage } from "../../pages/UsersPage";

export default function OrchestratorRoutes() {
  return (
    <Routes>
      <Route
        element={
          <AppShell
            navGroups={ORCHESTRATOR_NAV_GROUPS}
            allPages={ORCHESTRATOR_ALL_PAGES}
            commandActions={ORCHESTRATOR_COMMAND_ACTIONS}
            productSubtitle="流量编排器"
            productBadge="编排"
          />
        }
      >
        <Route index element={<OrchestratorOverviewPage />} />
        <Route path="topology" element={<OrchestratorTopologyPage />} />
        <Route path="groups" element={<OrchestratorGroupsPage />} />
        <Route path="policy" element={<OrchestratorPolicyPage />} />
        <Route path="transparent" element={<OrchestratorTransparentPage />} />
        <Route path="telemetry" element={<OrchestratorTelemetryPage />} />
        <Route path="runtime" element={<RuntimePage />} />
        <Route path="config" element={<ConfigPage />} />
        <Route path="upgrades" element={<OrchestratorUpgradesPage />} />
        <Route path="conversions" element={<OrchestratorConversionsPage />} />
        <Route path="system/users" element={<UsersPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
