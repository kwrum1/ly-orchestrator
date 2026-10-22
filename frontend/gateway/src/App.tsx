import { useEffect } from "react";
import { BrowserRouter, Navigate, Route, Routes, useLocation, useNavigate } from "react-router-dom";
import { AppShell } from "./app/AppShell";
import { LEGACY_ROUTES } from "./app/nav";
import { LoginPage } from "./pages/LoginPage";
import { OverviewPage } from "./pages/OverviewPage";
import { TrafficPage } from "./pages/TrafficPage";
import { ClientsPage } from "./pages/ClientsPage";
import { ConnectionsPage, DomainsPage } from "./pages/MonitorPages";
import { InterfacesPage } from "./pages/InterfacesPage";
import { WanPage } from "./pages/WanPage";
import { WanGroupsPage } from "./pages/WanGroupsPage";
import { DhcpPage } from "./pages/DhcpPage";
import { ProxyPage } from "./pages/ProxyPage";
import { RoutingPage } from "./pages/RoutingPage";
import { PortMappingPage } from "./pages/PortMappingPage";
import { DnsPage } from "./pages/DnsPage";
import { QosPage } from "./pages/QosPage";
import { DomainsObjectPage, IpObjectPage } from "./pages/ObjectsPage";
import { SecurityPage } from "./pages/SecurityPage";
import { RuntimePage } from "./pages/RuntimePage";
import { ConfigPage } from "./pages/ConfigPage";
import { FirmwarePage } from "./pages/FirmwarePage";
import { UsersPage } from "./pages/UsersPage";
import { useAuth } from "./state/providers";
import { Spinner } from "./components/ui";

function LegacyHashRedirect() {
  const navigate = useNavigate();
  useEffect(() => {
    const raw = window.location.hash.replace(/^#\/?/, "");
    if (!raw) return;
    const route = LEGACY_ROUTES[raw] || (raw.startsWith("/") ? raw : "");
    if (route) navigate(route, { replace: true });
  }, [navigate]);
  return null;
}

function NotFound() {
  const location = useLocation();
  return (
    <div className="not-found">
      <strong>页面不存在</strong>
      <p>{location.pathname}</p>
      <a href="/">返回总览</a>
    </div>
  );
}

function AuthGate() {
  const { session, loading, error } = useAuth();
  if (loading) {
    return <div className="app-loading"><Spinner label="正在读取会话…" /></div>;
  }
  if (!session) {
    return <LoginPage />;
  }
  if (session.password_change_required) {
    return <LoginPage forceChange initialUsername={session.username} />;
  }
  return (
    <Routes>
      <Route element={<AppShell />}>
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
        <Route path="*" element={<NotFound />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}

export function App() {
  return (
    <BrowserRouter>
      <LegacyHashRedirect />
      <AuthGate />
    </BrowserRouter>
  );
}
