import { StrictMode } from "react";
import { createRoot } from "react-dom/client";

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { TransportProvider } from "@connectrpc/connect-query";
import { platform } from "./lib/platform";
import { router } from "./router";
import "./console.css";

const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: 15_000 }, mutations: { retry: false, gcTime: 0 } } });

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}><TransportProvider transport={platform.transport}><RouterProvider router={router} /></TransportProvider></QueryClientProvider>
  </StrictMode>,
);
