import { Navigate, Route, Routes } from "react-router-dom";
import { AppShell } from "../../app/AppShell";
import { ALL_PAGES, COMMAND_ACTIONS, NAV_GROUPS } from "../../app/nav";
import { NotFoundPage } from "../../pages/NotFoundPage";
import { OverviewPage } from "../../pages/OverviewPage";
import { ClientsPage } from "../../pages/ClientsPage";
import { ConnectionsPage, DomainsPage } from "../../pages/MonitorPages";
import { InterfacesPage } from "../../pages/InterfacesPage";
import { WanPage } from "../../pages/WanPage";
import { WanGroupsPage } from "../../pages/WanGroupsPage";
import { DhcpPage } from "../../pages/DhcpPage";
import { ProxyPage } from "../../pages/ProxyPage";
import { RoutingPage } from "../../pages/RoutingPage";
import { PortMappingPage } from "../../pages/PortMappingPage";
import { DnsPage } from "../../pages/DnsPage";
import { QosPage } from "../../pages/QosPage";
import { ObjectsPage } from "../../pages/ObjectsPage";
import { ConfigPage } from "../../pages/ConfigPage";
import { FirmwarePage } from "../../pages/FirmwarePage";
import { AccountPage } from "../../pages/AccountPage";
import { DiagnosticsPage } from "../../pages/DiagnosticsPage";

export default function GatewayRoutes() {
  return (
    <Routes>
      <Route element={<AppShell navGroups={NAV_GROUPS} allPages={ALL_PAGES} commandActions={COMMAND_ACTIONS} productSubtitle="出口网关" />}>
        <Route index element={<OverviewPage />} />
        <Route path="clients" element={<ClientsPage />} />
        <Route path="connections" element={<ConnectionsPage />} />
        <Route path="domains" element={<DomainsPage />} />
        <Route path="interfaces" element={<InterfacesPage />} />
        <Route path="wan" element={<WanPage mode="wan" />} />
        <Route path="wan-groups" element={<WanGroupsPage />} />
        <Route path="lan" element={<WanPage mode="lan" />} />
        <Route path="dhcp" element={<DhcpPage />} />
        <Route path="proxy" element={<ProxyPage />} />
        <Route path="routing" element={<RoutingPage />} />
        <Route path="port-mapping" element={<PortMappingPage />} />
        <Route path="dns" element={<DnsPage />} />
        <Route path="qos" element={<QosPage />} />
        <Route path="objects" element={<ObjectsPage />} />
        <Route path="system/config" element={<ConfigPage />} />
        <Route path="system/firmware" element={<FirmwarePage />} />
        <Route path="system/account" element={<AccountPage />} />
        <Route path="system/diagnostics" element={<DiagnosticsPage />} />

        <Route path="traffic" element={<Navigate to="/" replace />} />
        <Route path="objects/domains" element={<Navigate to="/objects" replace />} />
        <Route path="objects/ips" element={<Navigate to="/objects" replace />} />
        <Route path="system/runtime" element={<Navigate to="/system/config" replace />} />
        <Route path="system/users" element={<Navigate to="/system/account" replace />} />
        <Route path="security" element={<Navigate to="/" replace />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
