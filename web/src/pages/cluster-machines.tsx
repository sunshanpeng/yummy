import { InventoryService } from "../gen/platform/v1/inventory_pb";
import { Button } from "../components/ui/button";
import { DataTable, Notice, QueryState } from "../components/console-kit";
import { formatTime } from "../lib/platform";
import { useRPC } from "../lib/queries";

export function ClusterMachines() {
  const query = useRPC(InventoryService.method.listClusterMachines, {}, { refetchInterval: 15000 });
  return <section className="section space-y-3">
    <div className="flex flex-wrap items-center justify-between gap-2">
      <h3 className="font-semibold">集群机器</h3>
      <Button size="sm" variant="outline" disabled={query.isFetching} onClick={() => void query.refetch()}>刷新机器清单</Button>
    </div>
    <QueryState query={query}>
      {query.data?.reason ? <Notice>{query.data.reason}</Notice> : <>
        <p className="text-xs text-muted-foreground">worker 最近上报于 {formatTime(query.data?.observedAt ?? "")}。</p>
        {query.data?.truncated && <Notice tone="warning">集群机器超过单次上报上限，当前显示前 256 台。</Notice>}
        <DataTable rows={query.data?.machines ?? []} label="集群机器清单" columns={[
          { id: "name", title: "机器", cell: (node) => <span className="break-all font-medium">{node.name}</span> },
          { id: "platform", title: "系统与架构", cell: (node) => <span>{node.os}/{node.arch}</span> },
          { id: "kubelet", title: "Kubelet 版本", cell: (node) => <span>{node.kubeletVersion || "—"}</span> },
          { id: "ready", title: "就绪状态", cell: (node) => <span>{node.ready ? "就绪" : "未就绪"}</span> },
          { id: "schedule", title: "调度封锁", cell: (node) => <span>{node.schedulable ? "未封锁" : "已封锁"}</span> },
        ]} />
      </>}
    </QueryState>
  </section>;
}
