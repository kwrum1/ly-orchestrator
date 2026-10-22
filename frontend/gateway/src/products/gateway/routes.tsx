import { Route, Routes } from "react-router-dom";
import { AppShell } from "../../app/AppShell";
import { ALL_PAGES, COMMAND_ACTIONS, NAV_GROUPS } from "../../app/nav";
import { NotFoundPage } from "../../pages/NotFoundPage";
import { OverviewPage } from "../../pages/OverviewPage";
import { TrafficPage } from "../../pages/TrafficPage";
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
import { DomainsObjectPage, IpObjectPage } from "../../pages/ObjectsPage";
import { SecurityPage } from "../../pages/SecurityPage";
import { RuntimePage } from "../../pages/RuntimePage";
import { ConfigPage } from "../../pages/ConfigPage";
import { FirmwarePage } from "../../pages/FirmwarePage";
import { UsersPage } from "../../pages/UsersPage";

export default function GatewayRoutes() {
  return (
    <Routes>
      <Route element={<AppShell navGroups={NAV_GROUPS} allPages={ALL_PAGES} commandActions={COMMAND_ACTIONS} productSubtitle="出口网关" />}>
        <Route index element={<OverviewPage />} />
        <Route path="traffic" element={<TrafficPage />} />
        <Route path="clients" element={<ClientsPage />} />
        <Route path="connections" element={<ConnectionsPage />} />
        <Route path="domains" element={<DomainsPage />} />
        <Route path="interfaces" element={<InterfacesPage />} />
        <Route path="wan" element={<WanPage />} />
        <Route path="wan-groups" element={<WanGroupsPage />} />
        <Route path="dhcp" element={<DhcpPage />} />
        <Route path="proxy" element={<ProxyPage />} />
        <Route path="routing" element={<RoutingPage />} />
        <Route path="port-mapping" element={<PortMappingPage />} />
        <Route path="dns" element={<DnsPage />} />
        <Route path="qos" element={<QosPage />} />
        <Route path="objects/domains" element={<DomainsObjectPage />} />
        <Route path="objects/ips" element={<IpObjectPage />} />
        <Route path="security" element={<SecurityPage />} />
        <Route path="system/runtime" element={<RuntimePage />} />
        <Route path="system/config" element={<ConfigPage />} />
        <Route path="system/firmware" element={<FirmwarePage />} />
        <Route path="system/users" element={<UsersPage />} />
        <Route path="*" element={<NotFoundPage />} />
      </Route>
    </Routes>
  );
}
