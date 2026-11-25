import type { RuntimeStatus } from "../gen/platform/v1/deployment_pb";
import { can } from "../lib/platform";
import { isRuntimeActive, runtimeStateLabel } from "../lib/runtime";
import { useApplication } from "../pages/workspaces";
import { Loading } from "./console-kit";
import { RuntimeLogsCard } from "./runtime-logs";
import { RuntimeMetricsCard } from "./runtime-metrics";

// 应用详情页“部署与运行”内的运行观察区块：日志查看 + 基础监控，全部来自真实 RPC；
// 未部署 / 已停止时给出明确空态，不渲染任何示例数据。
export function RuntimeObservation({ status, statusPending }: { status?: RuntimeStatus; statusPending?: boolean }) {
  const app = useApplication();
  if (!can(app, "runtime:read")) return null;
  if (statusPending && !status) return <Loading label="正在读取运行状态…" />;
  if (!isRuntimeActive(status)) {
    return (
      <section className="section" aria-label="运行日志与基础监控">
        <h3 className="section-title">运行日志与基础监控</h3>
        <p className="mt-3 text-sm text-muted-foreground">应用当前状态为「{runtimeStateLabel(status)}」，没有正在运行的实例，因此没有运行日志或监控采样。部署成功并启动后，这里展示 worker 上报的真实数据。</p>
      </section>
    );
  }
  return (
    <div className="stack">
      {can(app, "logs:read") && <RuntimeLogsCard />}
      <RuntimeMetricsCard />
    </div>
  );
}
