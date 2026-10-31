import { useLocation } from "react-router-dom";

export function NotFoundPage() {
  const location = useLocation();
  return (
    <div className="not-found">
      <strong>页面不存在</strong>
      <p>{location.pathname}</p>
      <a href="/">返回总览</a>
    </div>
  );
}
