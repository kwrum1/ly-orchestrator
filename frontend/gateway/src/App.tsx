import { lazy, Suspense, useEffect } from "react";
import { BrowserRouter, useNavigate } from "react-router-dom";
import { LEGACY_ROUTES } from "./app/nav";
import { ORCHESTRATOR_LEGACY_ROUTES } from "./app/orchestrator-nav";
import { LoginPage } from "./pages/LoginPage";
import { useAuth } from "./state/providers";
import { Spinner } from "./components/ui";
import { CURRENT_PRODUCT, PRODUCT } from "./product";

const ProductRoutes = PRODUCT === "orchestrator"
  ? lazy(() => import("./products/orchestrator/routes"))
  : lazy(() => import("./products/gateway/routes"));

const PRODUCT_LEGACY_ROUTES = PRODUCT === "orchestrator" ? ORCHESTRATOR_LEGACY_ROUTES : LEGACY_ROUTES;

function LegacyHashRedirect() {
  const navigate = useNavigate();
  useEffect(() => {
    const raw = window.location.hash.replace(/^#\/?/, "");
    if (!raw) return;
    const route = PRODUCT_LEGACY_ROUTES[raw] || (raw.startsWith("/") ? raw : "");
    if (route) navigate(route, { replace: true });
  }, [navigate]);
  return null;
}

function AuthGate() {
  const { session, loading } = useAuth();
  if (loading) {
    return <div className="app-loading"><Spinner label="正在读取会话…" /></div>;
  }
  if (!session) {
    return <LoginPage product={PRODUCT} />;
  }
  if (session.password_change_required) {
    return <LoginPage product={PRODUCT} forceChange initialUsername={session.username} />;
  }
  return (
    <Suspense fallback={<div className="app-loading"><Spinner label="正在加载控制台…" /></div>}>
      <ProductRoutes />
    </Suspense>
  );
}

export function App() {
  useEffect(() => {
    document.title = CURRENT_PRODUCT.documentTitle;
  }, []);

  return (
    <BrowserRouter>
      <LegacyHashRedirect />
      <AuthGate />
    </BrowserRouter>
  );
}
