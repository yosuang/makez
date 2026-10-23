import { createRootRoute, Outlet } from "@tanstack/react-router";

export const Route = createRootRoute({
  component: () => <Outlet />,
  notFoundComponent: () => (
    <main className="not-found">
      <h1>页面不存在</h1>
      <a href="/">返回工作台</a>
    </main>
  ),
});
