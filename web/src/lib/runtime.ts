import { createQueryOptions } from "@connectrpc/connect-query";
import { useInfiniteQuery, useQuery, useQueryClient, type InfiniteData } from "@tanstack/react-query";
import { useContext, useEffect, useState } from "react";
import { DeploymentService, type GetRuntimeLogsResponse, type RuntimeLogChunk, type RuntimeMetricSample, type RuntimeStatus } from "../gen/platform/v1/deployment_pb";
import { platform } from "./platform";
import { ScopeContext, useUser } from "./queries";

// 运行日志固定页大小；服务端按 (created_at, id) 时间正序返回键集游标分页，
// 首页是保留窗口内最早的日志块，nextCursor 指向更新的块。
export const runtimeLogPageSize = 50;

// 监控窗口选项（分钟）；since 在每次取数时计算，until 留空由服务端截到现在。
export const metricWindows = [
  { minutes: 30, label: "30 分钟" },
  { minutes: 60, label: "1 小时" },
  { minutes: 360, label: "6 小时" },
  { minutes: 1440, label: "24 小时" },
] as const;

// 筛选项变化时查询键改变，游标自动归零。
// 游标与服务端查询作用域绑定，跨过滤条件的游标会被服务端拒绝，因此过滤条件必须进查询键。
export function useRuntimeLogs(
  input: { workspaceId: string; applicationId: string; podUid: string; container: string; releaseId: string; since: string; until: string },
  options?: { enabled?: boolean; follow?: boolean },
) {
  const user = useUser();
  const scope = useContext(ScopeContext);
  const client = useQueryClient();
  const request = { ...input, limit: runtimeLogPageSize };
  const descriptor = createQueryOptions(
    DeploymentService.method.getRuntimeLogs,
    { ...request, cursor: "" },
    { transport: platform.transport },
  );
  const queryKey = ["scope", user.id, user.authVersion.toString(), scope, ...descriptor.queryKey];
  const pages = useInfiniteQuery({
    queryKey,
    queryFn: async ({ pageParam, signal }) => {
      const response = await platform.deployments.getRuntimeLogs(
        { ...request, cursor: pageParam },
        { signal },
      );
      return response;
    },
    initialPageParam: "",
    getNextPageParam: (lastPage) => lastPage.nextCursor || undefined,
    enabled: options?.enabled,
    retry: false,
  });
  const lastPage = pages.data?.pages.at(-1);
  const cursor = lastPage?.resumeCursor || lastPage?.nextCursor || "";
  const following = options?.follow === true && pages.isSuccess && !pages.isFetching;
  // 单独续读尾部。没有新日志时保留游标和已加载结果，避免反复下载全部历史页。
  const tail = useQuery({
    queryKey: [...queryKey, "follow", cursor],
    queryFn: ({ signal }) => platform.deployments.getRuntimeLogs({ ...request, cursor }, { signal }),
    enabled: following,
    refetchInterval: 2000,
    staleTime: 0,
    retry: false,
  });
  useEffect(() => {
    const next = tail.data;
    if (!following || !next?.chunks.length || !next.resumeCursor || next.resumeCursor === cursor) return;
    client.setQueryData<InfiniteData<GetRuntimeLogsResponse, string>>(queryKey, (current) => {
      const last = current?.pages.at(-1);
      if (!current || !last || (last.resumeCursor || last.nextCursor || "") !== cursor) return current;
      return { pages: [...current.pages, next].slice(-100), pageParams: [...current.pageParams, cursor].slice(-100) };
    });
  }, [client, cursor, following, queryKey, tail.data]);
  return { ...pages, followError: following ? tail.error : null };
}

// GetRuntimeMetrics：since 在取数时按窗口计算 RFC3339，查询键只含窗口档位，
// 避免毫秒级时间戳进入键导致每次渲染都视为新查询。
export function useRuntimeMetrics(
  input: { workspaceId: string; applicationId: string; windowMinutes: number },
  options?: { enabled?: boolean; refetchInterval?: number | false },
) {
  const user = useUser();
  const scope = useContext(ScopeContext);
  const descriptor = createQueryOptions(
    DeploymentService.method.getRuntimeMetrics,
    { workspaceId: input.workspaceId, applicationId: input.applicationId, since: "", until: "" },
    { transport: platform.transport },
  );
  return useQuery({
    queryKey: ["scope", user.id, user.authVersion.toString(), scope, ...descriptor.queryKey, { window: input.windowMinutes }],
    queryFn: ({ signal }) => {
      const since = new Date(Date.now() - input.windowMinutes * 60_000).toISOString();
      return platform.deployments.getRuntimeMetrics({ workspaceId: input.workspaceId, applicationId: input.applicationId, since, until: "" }, { signal });
    },
    enabled: options?.enabled,
    refetchInterval: options?.refetchInterval,
    retry: false,
  });
}

export type RuntimeLogBlock = {
  id: string;
  text: string;
  podUid: string;
  container: string;
  restartCount: number;
  truncated: boolean;
  createdAt: string;
};

// 同一容器实例连续解码；其他 Pod 或重启后的日志不会接续它的 UTF-8 字节。
export function decodeLogChunks(chunks: readonly RuntimeLogChunk[]): RuntimeLogBlock[] {
  const streams = new Map<string, { decoder: TextDecoder; last: number }>();
  const decoded = chunks.map((chunk, index) => {
    const key = JSON.stringify([chunk.podUid, chunk.container, chunk.restartCount]);
    let stream = streams.get(key);
    if (!stream) {
      stream = { decoder: new TextDecoder("utf-8"), last: index };
      streams.set(key, stream);
    }
    stream.last = index;
    return { chunk, text: stream.decoder.decode(chunk.content, { stream: true }) };
  });
  for (const { decoder, last } of streams.values()) decoded[last].text += decoder.decode();
  // 空内容块不渲染。
  return decoded.filter((item) => item.text !== "").map(({ chunk, text }) => ({
    id: chunk.id,
    text,
    podUid: chunk.podUid,
    container: chunk.container,
    restartCount: chunk.restartCount,
    truncated: chunk.truncated,
    createdAt: chunk.createdAt,
  }));
}

// worker.proto：cpu_nano_cores 是自上次采样窗口（cpu_window_millis）内的 CPU 纳秒消耗，
// 换算毫核 = 纳秒 / (窗口毫秒 × 1000)；内存直接按字节转 MiB。
export function cpuMilliCores(sample: Pick<RuntimeMetricSample, "cpuNanoCores" | "cpuWindowMillis">): number {
  const window = Number(sample.cpuWindowMillis);
  return window > 0 ? Number(sample.cpuNanoCores) / (window * 1000) : 0;
}
export function memoryMib(sample: Pick<RuntimeMetricSample, "memoryWorkingSetBytes">): number {
  return Number(sample.memoryWorkingSetBytes) / (1024 * 1024);
}

export function formatMilliCores(value: number): string {
  return value >= 1000 ? `${(value / 1000).toFixed(2)} 核` : `${Math.round(value)} 毫核`;
}
export function formatMib(value: number): string {
  return value >= 1024 ? `${(value / 1024).toFixed(2)} GiB` : `${Math.round(value * 10) / 10} MiB`;
}

export type MetricPoint = { time: number; cpu: number; memory: number };

// 汇总为两条时间序列（CPU / 内存）。多 Pod 时按秒对齐后求和，只做真实聚合，不补零。
export function buildMetricSeries(samples: readonly RuntimeMetricSample[]): { points: MetricPoint[]; podCount: number } {
  const buckets = new Map<number, MetricPoint>();
  const pods = new Set<string>();
  for (const sample of samples) {
    if (sample.podUid) pods.add(sample.podUid);
    const parsed = Date.parse(sample.collectedAt);
    if (!Number.isFinite(parsed)) continue;
    const time = Math.floor(parsed / 1000) * 1000;
    const bucket = buckets.get(time) ?? { time, cpu: 0, memory: 0 };
    bucket.cpu += cpuMilliCores(sample);
    bucket.memory += memoryMib(sample);
    buckets.set(time, bucket);
  }
  const points = [...buckets.values()].sort((a, b) => a.time - b.time);
  return { points, podCount: pods.size };
}

// 运行状态判定：NOT_DEPLOYED、空值视为未部署；STOPPED / STOPPING 视为未在运行。
// 仅这两种情况展示“应用未运行”空态，其余状态（含 UNKNOWN / UNHEALTHY / PENDING）照常请求真实数据。
export function isRuntimeActive(status: RuntimeStatus | undefined): boolean {
  const state = status?.observedState ?? "";
  return state !== "" && state !== "NOT_DEPLOYED" && state !== "STOPPED" && state !== "STOPPING";
}

export function runtimeStateLabel(status: RuntimeStatus | undefined): string {
  const state = status?.observedState ?? "";
  return ({ NOT_DEPLOYED: "尚未部署", STOPPED: "已停止", STOPPING: "正在停止" } as Record<string, string>)[state] ?? (state || "未知");
}

export function formatLogClock(value: string): string {
  const parsed = Date.parse(value);
  if (!Number.isFinite(parsed)) return "—";
  return new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false }).format(parsed);
}

export function formatMetricClock(time: number, withSeconds = false): string {
  return new Intl.DateTimeFormat("zh-CN", withSeconds ? { month: "numeric", day: "numeric", hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false } : { hour: "2-digit", minute: "2-digit", hour12: false }).format(time);
}

// 图表动画尊重系统的“减弱动态效果”设置；环境不支持 matchMedia 时按未开启处理。
export function usePrefersReducedMotion(): boolean {
  const [reduced, setReduced] = useState(() => typeof window !== "undefined" && typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
  useEffect(() => {
    if (typeof window.matchMedia !== "function") return;
    const media = window.matchMedia("(prefers-reduced-motion: reduce)");
    const onChange = () => setReduced(media.matches);
    media.addEventListener("change", onChange);
    return () => media.removeEventListener("change", onChange);
  }, []);
  return reduced;
}
