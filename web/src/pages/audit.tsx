import { DataTable, Empty, Pager, QueryState } from "../components/console-kit";
import { AuditService } from "../gen/platform/v1/audit_pb";
import { formatTime } from "../lib/platform";
import { useRPC } from "../lib/queries";

export function AuditEvents({ workspaceId = "", cursor, onPage }: {
  workspaceId?: string;
  cursor: string;
  onPage: (cursor: string) => void;
}) {
  const query = useRPC(AuditService.method.listAuditEvents, { workspaceId, limit: 24, cursor });
  const title = workspaceId ? "空间审计" : "全局审计";
  return <div className="stack">
    <h2 className="text-lg font-semibold">{title}</h2>
    <QueryState query={query}>
      {query.data?.events.length ? <DataTable label={title} rows={query.data.events} columns={[
        { id: "time", title: "时间", cell: (event) => <span className="whitespace-nowrap">{formatTime(event.createdAt)}</span> },
        { id: "actor", title: "操作者 ID", cell: (event) => <code className="text-xs">{event.actorId}</code> },
        { id: "action", title: "操作", cell: (event) => <span>{event.action}</span> },
        { id: "object", title: "对象", cell: (event) => <div className="space-y-1">
          <p>{event.objectType} <code className="text-xs">{event.objectId}</code></p>
          {!workspaceId && event.workspaceId && <p className="text-xs text-muted-foreground">空间 {event.workspaceId}</p>}
        </div> },
        { id: "result", title: "结果", cell: (event) => event.result === "SUCCESS" ? "已记录" : event.result },
        { id: "trace", title: "追踪 ID", cell: (event) => event.traceId ? <code className="text-xs">{event.traceId}</code> : "—" },
      ]} /> : <Empty title="暂无审计记录" description="操作记录会在这里显示。" />}
      {!!(cursor || query.data?.nextCursor) && <Pager cursor={cursor} nextCursor={query.data?.nextCursor} onPage={onPage} />}
    </QueryState>
  </div>;
}
