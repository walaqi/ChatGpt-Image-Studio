import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";

import App from "@/App";

// In production the app is mounted under /image-studio (same-origin reverse
// proxy). import.meta.env.BASE_URL is set from vite's `base`, so the router
// basename always matches the deployed asset prefix. Strip the trailing slash
// since BrowserRouter expects a basename without one.
const routerBasename = import.meta.env.BASE_URL.replace(/\/$/, "");

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <BrowserRouter basename={routerBasename}>
      <App />
    </BrowserRouter>
  </StrictMode>,
);
