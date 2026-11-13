import { useMemo, useRef, useState } from "react";
import { ArrowDown, ArrowDownToLine, LoaderCircle } from "lucide-react";
import { useApplication } from "../pages/workspaces";
import { errorMessage, formatTime } from "../lib/platform";
import { decodeLogChunks, formatLogClock, usePrefersReducedMotion, useRuntimeLogs } from "../lib/runtime";
import { Button } from "./ui/button";
import { Badge } from "./ui/badge";
import { Empty, Loading, Notice } from "./console-kit";

// 应用运行日志：服务端按 (created_at, id) 时间正序键集分页，
// 首页为保留窗口最早块，“加载更新的日志”沿 nextCursor 向后翻页；不补零、不造数据。
export function RuntimeLogsCard() {
  const app = useApplication();
  const [podUid, setPodUid] = useState("");
  const [container, setContainer] = useState("");
  const [showTime, setShowTime] = useState(true);
  const query = useRuntimeLogs({ workspaceId: app.workspaceId, applicationId: app.id, podUid, container });
  const chunks = useMemo(() => query.data?.pages.flatMap((page) => page.chunks) ?? [], [query.data]);
  const blocks = useMemo(() => decodeLogChunks(chunks), [chunks]);
  const podOptions = useMemo(() => [...new Set(chunks.map((chunk) => chunk.podUid).filter(Boolean))], [chunks]);
  const containerOptions = useMemo(() => [...new Set(chunks.map((chunk) => chunk.container).filter(Boolean))], [chunks]);
  const viewport = useRef<HTMLDivElement>(null);
  const reduced = usePrefersReducedMotion();

  function scrollToBottom() {
    const el = viewport.current;
    if (!el) return;
    if (typeof el.scrollTo === "function") el.scrollTo({ top: el.scrollHeight, behavior: reduced ? "auto" : "smooth" });
    else el.scrollTop = el.scrollHeight;
  }

  return (
    <section className="section" aria-label="运行日志">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="section-title">运行日志</h3>
          <p className="mt-1 text-sm text-muted-foreground">来自 worker 的真实日志块，按时间正序拼接；控制面不做任何补齐或示例填充。</p>
        </div>
        <label className="flex shrink-0 items-center gap-2 text-xs">
          <input type="checkbox" checked={showTime} onChange={(event) => setShowTime(event.target.checked)} />显示时间戳
        </label>
      </div>
      {(podOptions.length > 0 || containerOptions.length > 0) && (
        <div className="mt-4 flex flex-wrap items-center gap-3">
          {podOptions.length > 0 && (
            <select aria-label="按 Pod 过滤" className="min-h-8 w-auto max-w-[16rem] text-xs" value={podUid} onChange={(event) => setPodUid(event.target.value)}>
              <option value="">全部 Pod</option>
              {podOptions.map((value) => <option key={value} value={value}>{value}</option>)}
            </select>
          )}
          {containerOptions.length > 0 && (
            <select aria-label="按容器过滤" className="min-h-8 w-auto max-w-[16rem] text-xs" value={container} onChange={(event) => setContainer(event.target.value)}>
              <option value="">全部容器</option>
              {containerOptions.map((value) => <option key={value} value={value}>{value}</option>)}
            </select>
          )}
        </div>
      )}
      <div className="mt-4">
        {query.isPending ? (
          <Loading label="正在加载运行日志…" />
        ) : query.error ? (
          <div className="space-y-3">
            <Notice tone="error"><span className="flex items-start gap-2"><LoaderCircle className="mt-0.5 size-4 shrink-0" aria-hidden="true" />读取运行日志失败：{errorMessage(query.error)}</span></Notice>
            <Button variant="outline" onClick={() => void query.refetch()}>重新加载</Button>
          </div>
        ) : blocks.length === 0 ? (
          <Empty title="暂无运行日志" description="应用在运行中，但控制面还没有收到任何日志块；worker 上报后这里显示真实日志，不会补历史数据。" />
        ) : (
          <>
            <div ref={viewport} role="log" aria-label="应用运行日志" className="max-h-[420px] overflow-y-auto rounded-md border bg-muted/40 p-4 font-mono text-xs leading-relaxed">
              {blocks.map((block) => (
                <div key={block.id} className="flex gap-3 border-b border-border/40 py-1.5 last:border-b-0 last:pb-0">
                  {showTime && (
                    <time dateTime={block.createdAt} title={formatTime(block.createdAt)} className="w-[4.5rem] shrink-0 tabular-nums text-muted-foreground">
                      {formatLogClock(block.createdAt)}
                    </time>
                  )}
                  <div className="min-w-0 flex-1 space-y-1">
                    <p className="whitespace-pre-wrap break-words">{block.text}</p>
                    {(block.truncated || block.restartCount > 0) && (
                      <p className="flex flex-wrap gap-2 text-[11px] text-muted-foreground">
                        {block.restartCount > 0 && <Badge variant="outline">容器已重启 {block.restartCount} 次</Badge>}
                        {block.truncated && <span className="text-warning">该块超长已被截断</span>}
                      </p>
                    )}
                  </div>
                </div>
              ))}
            </div>
            <div className="mt-3 flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
              <span>已显示 {blocks.length} 个日志块（按时间正序）{query.hasNextPage ? "，服务端还有更多。" : "，已到保留窗口最新。"}</span>
              <div className="flex flex-wrap gap-2">
                <Button variant="outline" size="sm" aria-label="滚动到日志底部" onClick={scrollToBottom}>
                  <ArrowDownToLine />滚动到底部
                </Button>
                {query.hasNextPage && (
                  <Button variant="outline" size="sm" disabled={query.isFetchingNextPage} onClick={() => void query.fetchNextPage()}>
                    {query.isFetchingNextPage ? <><LoaderCircle className="animate-spin" />正在加载…</> : <><ArrowDown />加载更新的日志</>}
                  </Button>
                )}
              </div>
            </div>
          </>
        )}
      </div>
    </section>
  );
}
