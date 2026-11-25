import type { RuntimeConfigChange } from "../gen/platform/v1/runtime_config_pb";
import { Notice } from "./console-kit";

const fields: Record<string, string> = {
  type: "服务方式", port: "监听端口", probe: "健康探针", command: "启动命令",
  args: "启动参数", domains: "自定义域名", resources: "资源请求与限制", volumes: "持久卷",
  confirmed: "配置确认状态", variable: "运行变量", file: "配置文件",
};
const operations: Record<string, string> = { ADDED: "新增", UPDATED: "修改", REMOVED: "移除" };

export function RuntimeChanges({ changes }: { changes: RuntimeConfigChange[] }) {
  if (changes.length === 0) return <Notice>与当前生效版本的运行配置一致。</Notice>;
  return <ul aria-label="运行配置差异" className="divide-y rounded-md border text-sm">
    {changes.map(({ field, name, operation }) => <li key={JSON.stringify([field, name])} className="flex items-baseline justify-between gap-4 px-4 py-2">
      <span className="min-w-0 break-all">{fields[field] ?? field}{name && <> <code>{name}</code></>}</span>
      <span className="shrink-0 text-muted-foreground">{operations[operation] ?? operation}</span>
    </li>)}
  </ul>;
}
