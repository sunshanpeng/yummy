import { useState } from "react";
import { LoaderCircle } from "lucide-react";
import { Area, AreaChart, CartesianGrid, XAxis, YAxis } from "recharts";
import { useApplication } from "../pages/workspaces";
import { errorMessage } from "../lib/platform";
import { buildMetricSeries, formatMib, formatMetricClock, formatMilliCores, metricWindows, usePrefersReducedMotion, useRuntimeMetrics, type MetricPoint } from "../lib/runtime";
import { ChartContainer, ChartTooltip, ChartTooltipContent, type ChartConfig } from "./ui/chart";
import { Tabs, TabsList, TabsTrigger } from "./ui/tabs";
import { Button } from "./ui/button";
import { Empty, Loading, Notice } from "./console-kit";

const cpuConfig = { cpu: { label: "CPU", color: "var(--chart-1)" } } satisfies ChartConfig;
const memoryConfig = { memory: { label: "内存", color: "var(--chart-2)" } } satisfies ChartConfig;
const axisMilliCores = (value: number) => (value >= 1000 ? `${(value / 1000).toFixed(1)}核` : `${Math.round(value)}m`);
const axisMib = (value: number) => (value >= 1024 ? `${(value / 1024).toFixed(1)}Gi` : `${Math.round(value)}Mi`);

// 应用基础监控：CPU 与内存两条真实采样时间序列；数据只来自 GetRuntimeMetrics，不补零造点。
export function RuntimeMetricsCard() {
  const app = useApplication();
  const [windowMinutes, setWindowMinutes] = useState(60);
  const reduced = usePrefersReducedMotion();
  const query = useRuntimeMetrics({ workspaceId: app.workspaceId, applicationId: app.id, windowMinutes }, { refetchInterval: 30_000 });
  const series = query.data ? buildMetricSeries(query.data.samples) : undefined;
  const points = series?.points ?? [];

  return (
    <section className="section" aria-label="基础监控">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="section-title">基础监控</h3>
          <p className="mt-1 text-sm text-muted-foreground">CPU 与内存来自 worker 真实采样{series && series.podCount > 1 ? `；当前 ${series.podCount} 个 Pod 的样本按秒对齐求和` : ""}。</p>
        </div>
        <Tabs value={String(windowMinutes)} onValueChange={(value) => setWindowMinutes(Number(value))}>
          <TabsList aria-label="监控时间窗口" className="max-w-full overflow-x-auto">
            {metricWindows.map((option) => (
              <TabsTrigger key={option.minutes} value={String(option.minutes)} className="px-2.5 text-xs whitespace-nowrap">
                {option.label}
              </TabsTrigger>
            ))}
          </TabsList>
        </Tabs>
      </div>
      <div className="mt-4">
        {query.isPending ? (
          <Loading label="正在加载监控采样…" />
        ) : query.error ? (
          <div className="space-y-3">
            <Notice tone="error"><span className="flex items-start gap-2"><LoaderCircle className="mt-0.5 size-4 shrink-0" aria-hidden="true" />读取监控数据失败：{errorMessage(query.error)}</span></Notice>
            <Button variant="outline" onClick={() => void query.refetch()}>重新加载</Button>
          </div>
        ) : points.length === 0 ? (
          <Empty title="暂无监控数据" description="应用在运行中，但该窗口内还没有任何采样点；worker 开始上报后这里显示真实曲线，不会补零或造数据。" />
        ) : points.length === 1 ? (
          <SinglePointSummary point={points[0]} />
        ) : (
          <div className="grid gap-4 md:grid-cols-2">
            <div className="min-w-0 rounded-md border p-3">
              <div className="mb-1 flex items-center justify-between gap-2">
                <h4 className="text-sm font-medium">CPU</h4>
                <span className="text-xs text-muted-foreground">窗口内纳秒消耗换算毫核</span>
              </div>
              <ChartContainer config={cpuConfig} className="h-52 w-full sm:h-60">
                <AreaChart data={points} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                  <CartesianGrid vertical={false} stroke="var(--border)" />
                  <XAxis dataKey="time" type="number" domain={["dataMin", "dataMax"]} tickFormatter={(value) => formatMetricClock(Number(value))} minTickGap={40} tickLine={false} axisLine={false} tickMargin={6} />
                  <YAxis width={56} domain={[0, "auto"]} tickFormatter={(value) => axisMilliCores(Number(value))} tickLine={false} axisLine={false} tickMargin={4} />
                  <ChartTooltip
                    cursor={{ stroke: "var(--border)" }}
                    content={(
                      <ChartTooltipContent
                        labelFormatter={(_, payload) => formatMetricClock(Number((payload?.[0]?.payload as MetricPoint | undefined)?.time), true)}
                        formatter={(value) => (
                          <div className="flex min-w-0 flex-1 items-center justify-between gap-3">
                            <span className="text-muted-foreground">CPU 使用</span>
                            <span className="font-mono font-medium tabular-nums">{formatMilliCores(Number(value))}</span>
                          </div>
                        )}
                      />
                    )}
                  />
                  <Area dataKey="cpu" name="cpu" type="monotone" stroke="var(--color-cpu)" fill="var(--color-cpu)" fillOpacity={0.12} strokeWidth={1.5} dot={points.length <= 24 ? { r: 2, strokeWidth: 0, fill: "var(--color-cpu)" } : false} isAnimationActive={!reduced} />
                </AreaChart>
              </ChartContainer>
            </div>
            <div className="min-w-0 rounded-md border p-3">
              <div className="mb-1 flex items-center justify-between gap-2">
                <h4 className="text-sm font-medium">内存</h4>
                <span className="text-xs text-muted-foreground">工作集（MiB）</span>
              </div>
              <ChartContainer config={memoryConfig} className="h-52 w-full sm:h-60">
                <AreaChart data={points} margin={{ top: 8, right: 8, left: 0, bottom: 0 }}>
                  <CartesianGrid vertical={false} stroke="var(--border)" />
                  <XAxis dataKey="time" type="number" domain={["dataMin", "dataMax"]} tickFormatter={(value) => formatMetricClock(Number(value))} minTickGap={40} tickLine={false} axisLine={false} tickMargin={6} />
                  <YAxis width={56} domain={[0, "auto"]} tickFormatter={(value) => axisMib(Number(value))} tickLine={false} axisLine={false} tickMargin={4} />
                  <ChartTooltip
                    cursor={{ stroke: "var(--border)" }}
                    content={(
                      <ChartTooltipContent
                        labelFormatter={(_, payload) => formatMetricClock(Number((payload?.[0]?.payload as MetricPoint | undefined)?.time), true)}
                        formatter={(value) => (
                          <div className="flex min-w-0 flex-1 items-center justify-between gap-3">
                            <span className="text-muted-foreground">内存工作集</span>
                            <span className="font-mono font-medium tabular-nums">{formatMib(Number(value))}</span>
                          </div>
                        )}
                      />
                    )}
                  />
                  <Area dataKey="memory" name="memory" type="monotone" stroke="var(--color-memory)" fill="var(--color-memory)" fillOpacity={0.12} strokeWidth={1.5} dot={points.length <= 24 ? { r: 2, strokeWidth: 0, fill: "var(--color-memory)" } : false} isAnimationActive={!reduced} />
                </AreaChart>
              </ChartContainer>
            </div>
          </div>
        )}
      </div>
    </section>
  );
}

function SinglePointSummary({ point }: { point: MetricPoint }) {
  return (
    <div className="rounded-md border p-4 text-sm">
      <p>窗口内只有 1 个采样点（{formatMetricClock(point.time, true)}）：CPU {formatMilliCores(point.cpu)}，内存 {formatMib(point.memory)}。</p>
      <p className="mt-1 text-xs text-muted-foreground">样本不足以绘制趋势图；展示真实读数，不补零造点。</p>
    </div>
  );
}
