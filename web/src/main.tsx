import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import "./lib/style.css";
import { GoGate } from "./lib/ui";
import App from "./App";

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <GoGate>
      <App />
    </GoGate>
  </StrictMode>,
);
