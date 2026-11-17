import { useId, useRef, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2, Wand2 } from "lucide-react";
import { z } from "zod";
import { InventoryService } from "../gen/platform/v1/inventory_pb";
import { RuntimeConfigService, type RuntimeConfig } from "../gen/platform/v1/runtime_config_pb";
import { invalidateMethods, useRPC } from "../lib/queries";
import { useParams } from "@tanstack/react-router";
import { can, platform } from "../lib/platform";
import { useApplication } from "./workspaces";
import { deploySubNav } from "./deployments";
import { DataForm, Notice, QueryState, type FieldDefinition } from "../components/console-kit";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Textarea } from "../components/ui/textarea";
import { Label } from "../components/ui/label";
import { Badge } from "../components/ui/badge";

type Variable = { name: string; value: string; sensitive: boolean; keepExisting: boolean; key: string };
type ConfigFile = { path: string; content: string; sensitive: boolean; keepExisting: boolean; key: string };
type Volume = { name: string; mountPath: string; sizeGiB: string; key: string };
const integer = (min: number, max: number) => z.string().regex(/^\d+$/, "请输入整数").refine((value) => Number(value) >= min && Number(value) <= max, `请输入 ${min}～${max} 的整数`);
const stringList = z.string().refine((value) => { try { const data = JSON.parse(value || "[]"); return Array.isArray(data) && data.every((item) => typeof item === "string"); } catch { return false; } }, '请输入字符串 JSON 数组，例如 ["node", "server.js"]');
const parseList = (value: string): string[] => JSON.parse(value || "[]") as string[];
export function RuntimePage() {
  const app = useApplication();
  const query = useRPC(RuntimeConfigService.method.getRuntimeConfig, { workspaceId: app.workspaceId, applicationId: app.id }, { enabled: can(app, "config:read") });
  if (!can(app, "config:read")) return <Notice>当前账户没有查看此应用配置的权限。</Notice>;
  const { workspaceSlug } = useParams({ strict: false }) as { workspaceSlug: string };
  return <div className="stack">{deploySubNav(app, workspaceSlug)}<QueryState query={query} empty={!query.data?.config}>{query.data?.config && <RuntimeEditor key={`${app.id}:${query.data.config.revision}`} config={query.data.config} />}</QueryState></div>;
}
// 运行模板：按语言/形态给出建议值（端口、探针、启动方式），用户在此基础上
// 调整后保存；模板只预填表单值，不新增服务端配置。仅首次进入（尚未保存过
// 配置）时展示，已有配置直接呈现当前值。
const templates = [
  { id: "go-http", name: "Go HTTP 服务", description: "监听 8080，探针 /health，启动命令继承镜像默认。", type: "HTTP", port: "8080", probePath: "/health" },
  { id: "node-http", name: "Node.js 服务", description: "监听 3000，探针 /healthz。", type: "HTTP", port: "3000", probePath: "/healthz" },
  { id: "static", name: "静态站点 / 现成镜像", description: "监听 8080，探针 /（能响应即健康）。", type: "HTTP", port: "8080", probePath: "/" },
  { id: "worker", name: "后台进程", description: "不监听端口；无探针与域名，适合消费者/定时任务。", type: "WORKER", port: "", probePath: "" },
];
function RuntimeEditor({ config }: { config: RuntimeConfig }) {
  const app = useApplication(); const client = useQueryClient(); const readOnly = !can(app, "config:write");
  const [type, setType] = useState(config.type || "HTTP"); const [confirmed, setConfirmed] = useState(config.confirmed);
  const [formSeed, setFormSeed] = useState(0); const [template, setTemplate] = useState("");
  const [variables, setVariables] = useState<Variable[]>(() => config.variables.map((v) => ({ ...v, key: crypto.randomUUID(), keepExisting: v.sensitive && v.configured })));
  const [files, setFiles] = useState<ConfigFile[]>(() => config.files.map((f) => ({ ...f, key: crypto.randomUUID(), keepExisting: f.sensitive && f.configured })));
  const [volumes, setVolumes] = useState<Volume[]>(() => config.volumes.map((v) => ({ ...v, key: crypto.randomUUID(), sizeGiB: v.sizeGib.toString() })));
  const resources = config.resources;
  // 探针节奏与资源限额是高级项：默认值已可用，默认折叠避免淹没主要流程。
  const [probe, setProbe] = useState({ initialDelay: String(config.probe?.initialDelaySeconds ?? 0), period: String(config.probe?.periodSeconds || 10), timeout: String(config.probe?.timeoutSeconds || 2), failureThreshold: String(config.probe?.failureThreshold || 3) });
  const [resource, setResource] = useState({ cpuRequest: String(resources?.cpuRequestMilli ?? 100n), cpuLimit: String(resources?.cpuLimitMilli ?? 500n), memoryRequest: String(resources?.memoryRequestMib ?? 128n), memoryLimit: String(resources?.memoryLimitMib ?? 512n) });
  const idempotency = useRef({ fingerprint: "", key: "" });
  const execution = useRPC(InventoryService.method.resolveExecution, { workspaceId: app.workspaceId, applicationId: app.id, expectedPolicyRevision: 0n, runtime: true });
  const applyTemplate = (item: (typeof templates)[number]) => {
    setType(item.type); setTemplate(item.id); setFormSeed((seed) => seed + 1);
    if (item.type === "HTTP") setProbe({ initialDelay: "0", period: "10", timeout: "2", failureThreshold: "3" });
  };
  const fields: FieldDefinition[] = [
    ...(type === "HTTP" ? [
      { name: "port", label: "容器监听端口", type: "number", required: true, value: template === "go-http" ? "8080" : template === "node-http" ? "3000" : template === "static" ? "8080" : String(config.port || 8080), schema: integer(1, 65535), hint: "必填；应用容器内监听的端口。默认 8080，可修改。" },
      { name: "probePath", label: "健康探针路径", value: template ? templates.find((item) => item.id === template)?.probePath : config.probe?.path || "/health", schema: z.string().startsWith("/", "请输入以 / 开头的路径").max(1024).refine((value) => !/[?#\r\n]/.test(value), "不能包含查询参数、片段或换行"), hint: "选填；留空表示不做 HTTP 健康检查。填 /health 等返回 2xx 的路径，该路径同时用于启动、就绪与存活判定。" },
      { name: "domains", label: "自定义域名", value: config.domains.join("\n"), type: "textarea", hint: "选填；每行一个完整域名，不含协议或路径。默认使用平台自动分配地址，仅保存配置，不创建 DNS、证书或入口。" },
    ] : []),
    { name: "command", label: "启动命令", type: "textarea", value: JSON.stringify(config.command), schema: stringList, hint: "选填；字符串 JSON 数组，不经过 Shell。留空继承镜像默认命令，例如 [\"/app/server\"]。" },
    { name: "args", label: "启动参数", type: "textarea", value: JSON.stringify(config.args), schema: stringList, hint: "选填；每个数组元素作为独立参数，不执行字符串拼接。" },
  ];
  return <div className="stack max-w-4xl"><div className="flex flex-wrap items-center justify-between gap-3"><div><h2 className="text-lg font-semibold">运行配置</h2><p className="mt-1 text-sm text-muted-foreground">{config.revision === 0n ? "当前为系统建议，尚未保存。" : `最新已保存配置 v${config.revision}。此版本不代表正在运行。`}</p></div><Badge variant="outline">{config.revision === 0n ? "待确认建议" : config.confirmed ? "已确认配置" : "未确认草稿"}</Badge></div><Notice tone="warning">保存只创建配置版本，不会构建或部署。部署需在应用部署页明确确认；部署后可在部署页查看运行日志与基础监控。</Notice>{readOnly && <Notice>你可以查看配置，但没有修改权限。敏感内容始终不回显。</Notice>}
  {config.revision === 0n && !template && !readOnly && <section className="section"><div className="mb-4 flex items-center gap-2"><Wand2 className="size-4 text-muted-foreground" /><h3 className="section-title mb-0">从模板开始</h3><span className="text-xs text-muted-foreground">选择最接近的形态，之后可以逐项调整；也可以直接跳过。</span></div><div className="grid gap-3 sm:grid-cols-2">{templates.map((item) => <button key={item.id} type="button" onClick={() => applyTemplate(item)} className="cursor-pointer rounded-lg border bg-card p-4 text-left transition-colors hover:border-input hover:bg-muted/40"><span className="block font-medium">{item.name}</span><span className="mt-1 block text-xs text-muted-foreground">{item.description}</span></button>)}</div><p className="mt-3 text-xs text-muted-foreground">不使用模板？表单已按默认值填好，直接检查并保存即可。</p></section>}
  {template && <Notice tone="success">已按「{templates.find((item) => item.id === template)?.name}」预填，检查并调整后保存即可。</Notice>}
  <QueryState query={execution}>{execution.data?.summary && <section className="section"><h3 className="section-title mb-4">平台托管摘要（只读）</h3><dl className="metadata"><div><dt>默认地址</dt><dd>{execution.data.summary.defaultDomain || "尚未配置"}</dd></div><div><dt>目标架构</dt><dd>{execution.data.summary.platform || "尚未确定"}</dd></div><div><dt>平台策略版本</dt><dd>v{execution.data.summary.policyRevision}</dd></div><div><dt>执行资格</dt><dd>{execution.data.summary.ready ? "当前检查通过" : execution.data.summary.reason || "未就绪"}</dd></div></dl><p className="mt-4 text-xs text-muted-foreground">此摘要不允许覆盖节点、仓库与架构，也不表示已创建部署或迁移已有 Placement。</p></section>}</QueryState>
  <section className="section"><div className="mb-6 space-y-2"><Label htmlFor="runtime-type">服务方式</Label><select id="runtime-type" disabled={readOnly} className="block w-full sm:max-w-xs" value={type} onChange={(event) => { setType(event.target.value); setTemplate(""); }}><option value="HTTP">HTTP 服务：对外提供网页或 API，需要端口与域名</option><option value="WORKER">后台进程：不对外监听，适合消费者与定时任务</option></select><p className="text-xs text-muted-foreground">带 <span aria-hidden="true" className="text-destructive">*</span> 为必填；其余字段都已带默认值或可选。</p></div><DataForm key={`${type}:${formSeed}`} groups={[
    ...(type === "HTTP" ? [{ title: "访问入口", description: "应用如何被访问。端口必填，域名可选（默认自动分配平台地址）。", fields: [fields[0], fields[1]] }] : []),
    { title: "启动方式", description: "如何启动应用；两者都留空则完全继承镜像默认。", fields: type === "HTTP" ? [fields[3], fields[4]] : [fields[0], fields[1]] },
    ...(type === "HTTP" ? [{ title: "健康检查", description: "平台把这条探针同时用于三种判定：启动保护（启动超时判失败）、就绪（不就绪不接流量、滚动发布新版本不通过不上线）、存活（持续失败重启容器）。只需要一个能返回 2xx 的路径；如需为就绪与存活配置不同路径与节奏，属于后续扩展。", fields: [fields[2]] }] : []),
  ]} disabled={readOnly} submitLabel="保存运行配置" onSubmit={async (value) => {
    if (Number(resource.cpuRequest) > Number(resource.cpuLimit) || Number(resource.memoryRequest) > Number(resource.memoryLimit)) throw new Error("资源请求不能超过相应限制。");
    const variableNames = new Set<string>();
    for (const item of variables) { if (!/^[A-Za-z_][A-Za-z0-9_]{0,127}$/.test(item.name) || variableNames.has(item.name)) throw new Error("变量名称格式错误或重复，请检查变量列表。"); variableNames.add(item.name); }
    for (const volume of volumes) if (!/^\d+$/.test(volume.sizeGiB) || Number(volume.sizeGiB) < 1 || Number(volume.sizeGiB) > 100) throw new Error("持久卷容量必须为 1～100 GiB 的整数。");
    const payload = { workspaceId: app.workspaceId, applicationId: app.id, expectedRevision: config.revision, confirmed, type,
      port: type === "HTTP" ? Number(value.port) : 0,
      probe: type === "HTTP" && value.probePath ? { path: value.probePath, initialDelaySeconds: Number(probe.initialDelay), periodSeconds: Number(probe.period), timeoutSeconds: Number(probe.timeout), failureThreshold: Number(probe.failureThreshold) } : {},
      domains: type === "HTTP" ? (value.domains ?? "").split("\n").map((item) => item.trim()).filter(Boolean) : [], command: parseList(value.command), args: parseList(value.args),
      resources: { cpuRequestMilli: BigInt(resource.cpuRequest), cpuLimitMilli: BigInt(resource.cpuLimit), memoryRequestMib: BigInt(resource.memoryRequest), memoryLimitMib: BigInt(resource.memoryLimit) },
      variables: variables.map((item) => ({ name: item.name, sensitive: item.sensitive, keepExisting: item.keepExisting, value: item.keepExisting ? "" : item.value })),
      files: files.map((item) => ({ path: item.path, sensitive: item.sensitive, keepExisting: item.keepExisting, content: item.keepExisting ? "" : item.content })),
      volumes: volumes.map((item) => ({ name: item.name, mountPath: item.mountPath, sizeGib: BigInt(item.sizeGiB) })),
    };
    const fingerprint = JSON.stringify(payload, (_, value) => typeof value === "bigint" ? value.toString() : value);
    if (fingerprint !== idempotency.current.fingerprint) idempotency.current = { fingerprint, key: crypto.randomUUID() };
    const response = await platform.runtime.saveRuntimeConfig({ ...payload, idempotencyKey: idempotency.current.key });
    if (!response.config) throw new Error("服务端未返回保存结果，请重新加载以确认版本，不要直接重复提交。");
    // 先删除本地输入中的秘密，再重新获取只读配置摘要。
    setVariables((items) => items.map((item) => item.sensitive ? { ...item, value: "", keepExisting: true } : item));
    setFiles((items) => items.map((item) => item.sensitive ? { ...item, content: "", keepExisting: true } : item));
    await invalidateMethods(client, ["GetRuntimeConfig"]);
  }}>
    {type === "HTTP" && <details className="rounded-md border p-4"><summary className="cursor-pointer text-sm font-medium">探针高级参数（默认已可用，一般无需调整）</summary><div className="mt-4 grid gap-4 sm:grid-cols-2"><EditorInput label="初始延迟（秒）" type="number" value={probe.initialDelay} disabled={readOnly} onChange={(initialDelay) => setProbe({ ...probe, initialDelay })} /><EditorInput label="探测间隔（秒）" type="number" value={probe.period} disabled={readOnly} onChange={(period) => setProbe({ ...probe, period })} /><EditorInput label="单次超时（秒）" type="number" value={probe.timeout} disabled={readOnly} onChange={(timeout) => setProbe({ ...probe, timeout })} /><EditorInput label="连续失败阈值" type="number" value={probe.failureThreshold} disabled={readOnly} onChange={(failureThreshold) => setProbe({ ...probe, failureThreshold })} /></div><p className="mt-3 text-xs text-muted-foreground">默认：启动后 0 秒开始探测，每 10 秒一次，2 秒超时，连续失败 3 次判定不健康。同一组参数用于启动/就绪/存活三种探针；启动保护额外放宽为连续失败 30 次，即应用最长有约 5 分钟启动时间（探测间隔 × 30），超时判启动失败并重启。</p></details>}
    <details className="rounded-md border p-4"><summary className="cursor-pointer text-sm font-medium">资源限额（默认已可用，一般无需调整）</summary><div className="mt-4 grid gap-4 sm:grid-cols-2"><EditorInput label="CPU 请求（毫核）" type="number" value={resource.cpuRequest} disabled={readOnly} onChange={(cpuRequest) => setResource({ ...resource, cpuRequest })} /><EditorInput label="CPU 限制（毫核）" type="number" value={resource.cpuLimit} disabled={readOnly} onChange={(cpuLimit) => setResource({ ...resource, cpuLimit })} /><EditorInput label="内存请求（MiB）" type="number" value={resource.memoryRequest} disabled={readOnly} onChange={(memoryRequest) => setResource({ ...resource, memoryRequest })} /><EditorInput label="内存限制（MiB）" type="number" value={resource.memoryLimit} disabled={readOnly} onChange={(memoryLimit) => setResource({ ...resource, memoryLimit })} /></div><p className="mt-3 text-xs text-muted-foreground">默认：CPU 100～500 毫核，内存 128～512 MiB。请求不足时应用可能被节点挤压，限制超出会触发平台策略校验。</p></details>
    <div className="border-t pt-6"><div className="mb-4 flex flex-wrap items-center justify-between gap-3"><div><h3 className="font-semibold">环境变量 <span className="text-xs font-normal text-muted-foreground">（选填）</span></h3><p className="text-xs text-muted-foreground">普通值可编辑，敏感值只替换或明确保留。</p></div><Button type="button" variant="outline" size="sm" disabled={readOnly || variables.length >= 200} onClick={() => setVariables([...variables, { key: crypto.randomUUID(), name: "", value: "", sensitive: false, keepExisting: false }])}><Plus />添加变量</Button></div>{!variables.length && <p className="text-sm text-muted-foreground">尚未添加变量。</p>}<div className="space-y-4">{variables.map((item, index) => <div key={item.key} className="space-y-3 rounded-md border p-4"><div className="grid gap-3 sm:grid-cols-2"><EditorInput label={`变量 ${index + 1} 名称`} value={item.name} disabled={readOnly || item.keepExisting} onChange={(name) => setVariables(variables.map((v) => v.key === item.key ? { ...v, name } : v))} /><EditorInput label={`变量 ${index + 1} 值`} value={item.keepExisting ? "" : item.value} type={item.sensitive ? "password" : "text"} disabled={readOnly || item.keepExisting} placeholder={item.keepExisting ? "已配置，保存时保留原值" : "输入值"} onChange={(value) => setVariables(variables.map((v) => v.key === item.key ? { ...v, value } : v))} /></div><div className="flex flex-wrap items-center gap-4"><label className="flex items-center gap-2 text-xs"><input type="checkbox" disabled={readOnly || item.keepExisting} checked={item.sensitive} onChange={(event) => setVariables(variables.map((v) => v.key === item.key ? { ...v, sensitive: event.target.checked } : v))} />敏感值</label>{item.keepExisting && <Button type="button" variant="outline" size="sm" disabled={readOnly} onClick={() => setVariables(variables.map((v) => v.key === item.key ? { ...v, keepExisting: false, value: "" } : v))}>替换敏感值</Button>}<Button type="button" variant="ghost" size="sm" disabled={readOnly} onClick={() => setVariables(variables.filter((v) => v.key !== item.key))}><Trash2 />移除变量 {index + 1}</Button></div></div>)}</div></div>
    <div className="border-t pt-6"><div className="mb-4 flex flex-wrap items-center justify-between gap-3"><div><h3 className="font-semibold">配置文件 <span className="text-xs font-normal text-muted-foreground">（选填）</span></h3><p className="text-xs text-muted-foreground">只读挂载；敏感内容不回显。单个文件最多 1 MiB。</p></div><Button type="button" variant="outline" size="sm" disabled={readOnly || files.length >= 50} onClick={() => setFiles([...files, { key: crypto.randomUUID(), path: "", content: "", sensitive: false, keepExisting: false }])}><Plus />添加文件</Button></div>{!files.length && <p className="text-sm text-muted-foreground">尚未添加配置文件。</p>}<div className="space-y-4">{files.map((item, index) => <div key={item.key} className="space-y-3 rounded-md border p-4"><EditorInput label={`文件 ${index + 1} 挂载路径`} value={item.path} disabled={readOnly || item.keepExisting} placeholder="/app/config/settings.json" onChange={(path) => setFiles(files.map((v) => v.key === item.key ? { ...v, path } : v))} /><EditorInput label={`文件 ${index + 1} 内容`} value={item.keepExisting ? "" : item.content} textarea disabled={readOnly || item.keepExisting} placeholder={item.keepExisting ? "已配置，保存时保留原内容" : "输入文件内容"} onChange={(content) => setFiles(files.map((v) => v.key === item.key ? { ...v, content } : v))} /><div className="flex flex-wrap items-center gap-4"><label className="flex items-center gap-2 text-xs"><input type="checkbox" checked={item.sensitive} disabled={readOnly || item.keepExisting} onChange={(event) => setFiles(files.map((v) => v.key === item.key ? { ...v, sensitive: event.target.checked } : v))} />敏感文件</label>{item.keepExisting && <Button type="button" variant="outline" size="sm" disabled={readOnly} onClick={() => setFiles(files.map((v) => v.key === item.key ? { ...v, content: "", keepExisting: false } : v))}>替换文件内容</Button>}<Button type="button" variant="ghost" size="sm" disabled={readOnly} onClick={() => setFiles(files.filter((v) => v.key !== item.key))}><Trash2 />移除文件 {index + 1}</Button></div></div>)}</div></div>
    <div className="border-t pt-6"><div className="mb-4 flex flex-wrap items-center justify-between gap-3"><div><h3 className="font-semibold">持久卷 <span className="text-xs font-normal text-muted-foreground">（选填）</span></h3><p className="text-xs text-muted-foreground">解除挂载仅改变配置，不表示删除底层数据。当前尚未创建运行存储资源。</p></div><Button type="button" variant="outline" size="sm" disabled={readOnly || volumes.length >= 10} onClick={() => setVolumes([...volumes, { key: crypto.randomUUID(), name: "", mountPath: "", sizeGiB: "1" }])}><Plus />添加卷</Button></div>{!volumes.length && <p className="text-sm text-muted-foreground">尚未添加持久卷。</p>}<div className="space-y-4">{volumes.map((item, index) => <div className="space-y-3 rounded-md border p-4" key={item.key}><div className="grid gap-3 sm:grid-cols-3"><EditorInput label={`卷 ${index + 1} 名称`} value={item.name} disabled={readOnly} onChange={(name) => setVolumes(volumes.map((v) => v.key === item.key ? { ...v, name } : v))} /><EditorInput label={`卷 ${index + 1} 挂载路径`} value={item.mountPath} disabled={readOnly} onChange={(mountPath) => setVolumes(volumes.map((v) => v.key === item.key ? { ...v, mountPath } : v))} /><EditorInput label={`卷 ${index + 1} 容量（GiB）`} type="number" value={item.sizeGiB} disabled={readOnly} onChange={(sizeGiB) => setVolumes(volumes.map((v) => v.key === item.key ? { ...v, sizeGiB } : v))} /></div><Button type="button" variant="ghost" size="sm" disabled={readOnly} onClick={() => setVolumes(volumes.filter((v) => v.key !== item.key))}><Trash2 />移除卷 {index + 1}</Button></div>)}</div></div>
    <label className="flex items-start gap-3 rounded-md border bg-muted p-4"><input className="mt-1 size-4 accent-primary" type="checkbox" checked={confirmed} disabled={readOnly} onChange={(event) => setConfirmed(event.target.checked)} /><span><span className="font-medium">确认运行配置</span><span className="mt-1 block text-xs text-muted-foreground">确认此配置用于后续首次部署的预检；未勾选则保存为未确认草稿。无论是否确认，保存均不触发部署。</span></span></label>
  </DataForm></section></div>;
}
function EditorInput({ label, value, onChange, disabled, type = "text", placeholder, textarea }: { label: string; value: string; onChange: (value: string) => void; disabled?: boolean; type?: string; placeholder?: string; textarea?: boolean }) {
  const id = useId(); return <div className="min-w-0 space-y-2"><Label htmlFor={id}>{label}</Label>{textarea ? <Textarea id={id} value={value} disabled={disabled} rows={4} placeholder={placeholder} autoComplete="off" onChange={(event) => onChange(event.target.value)} /> : <Input id={id} value={value} disabled={disabled} type={type} placeholder={placeholder} autoComplete="off" onChange={(event) => onChange(event.target.value)} />}</div>;
}
