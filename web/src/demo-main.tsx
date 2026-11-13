import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { App } from "./App";
import { controlApi } from "./api";
import "./styles.css";

// 原 uppercase 演示保留为独立入口，不属于正式业务控制台。
createRoot(document.getElementById("root")!).render(<StrictMode><App api={controlApi} /></StrictMode>);
