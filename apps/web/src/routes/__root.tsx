import { createRootRoute, Outlet } from "@tanstack/react-router";

export const Route = createRootRoute({
  component: () => <Outlet />,
  notFoundComponent: () => (
    <main className="p-16">
      <h1 className="text-lg font-medium">Page not found</h1>
      <a className="text-sm text-foreground underline-offset-4 hover:underline" href="/">
        Back to the workspace
      </a>
    </main>
  ),
});
