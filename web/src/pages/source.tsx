import { useEffect, useRef, useState } from "react";
import { Link, useParams } from "@tanstack/react-router";
import { Upload, Check } from "lucide-react";
import { z } from "zod";
import { SourceService, type SourceSnapshot } from "../gen/platform/v1/source_pb";
import { useRPC } from "../lib/queries";
import { can, errorMessage, formatTime, platform } from "../lib/platform";
import { useApplication } from "./workspaces";
import { DataForm, DataTable, Notice, QueryState, required } from "../components/console-kit";
import { Button } from "../components/ui/button";
import { Input } from "../components/ui/input";
import { Label } from "../components/ui/label";

export function SourcePage() {
  const app = useApplication(); const { workspaceSlug } = useParams({ strict: false }) as { workspaceSlug: string }; const [file, setFile] = useState<File>(); const [stage, setStage] = useState(""); const [snapshot, setSnapshot] = useState<SourceSnapshot>();
  const controller = useRef<AbortController | undefined>(undefined);
  const session = useRef<{ fingerprint: string; uploadId: string; transferPath: string; transferred: boolean } | undefined>(undefined);
  useEffect(() => () => controller.current?.abort(), []);
  if (!can(app, "source:upload")) return <Notice>当前账户没有上传此应用源码的权限。</Notice>;
  return <div className="stack max-w-4xl"><div><h2 className="text-lg font-semibold">源码上传与校验</h2><p className="mt-1 text-sm text-muted-foreground">上传当前应用的 tar.gz 源码包，由控制面校验摘要和 Dockerfile。不会自动创建构建或部署。</p></div><Notice>上传完成只表示源码快照已准备好，不代表镜像产出或应用上线。请到应用构建页预览输入后明确创建构建。</Notice><section className="section"><DataForm submitLabel="上传并校验源码" fields={[{ name: "dockerfile", label: "Dockerfile 相对路径", value: "Dockerfile", schema: required.refine((value) => !value.startsWith("/") && !value.split("/").includes("..") && !value.includes("\\"), "使用源码包内的安全相对路径") }]} onSubmit={async (values) => {
    if (!file) throw new Error("请先选择 tar.gz 源码包。");
    if (file.size <= 0 || file.size > 256 * 1024 * 1024) throw new Error("源码包必须大于 0 且不超过 256 MiB；实例可能设有更严格的限制。");
    if (!file.name.endsWith(".tar.gz") && !file.name.endsWith(".tgz")) throw new Error("请选择 .tar.gz 或 .tgz 格式源码包。");
    const abort = new AbortController(); controller.current = abort;
    try {
      setStage("正在计算 SHA-256 摘要…");
      const sha256 = Array.from(new Uint8Array(await crypto.subtle.digest("SHA-256", await file.arrayBuffer()))).map((value) => value.toString(16).padStart(2, "0")).join("");
      if (abort.signal.aborted) throw new Error("已停止等待上传。");
      const fingerprint = `${sha256}:${file.size}:${values.dockerfile}`;
      if (session.current?.fingerprint !== fingerprint) {
        setStage("正在申请上传会话…");
        const response = await platform.sources.createSourceUpload({ workspaceId: app.workspaceId, applicationId: app.id, size: BigInt(file.size), sha256, dockerfile: values.dockerfile }, { signal: abort.signal });
        if (!response.upload || !response.transferPath) throw new Error("控制面没有返回完整上传会话。");
        session.current = { fingerprint, uploadId: response.upload.id, transferPath: response.transferPath, transferred: false };
      }
      const upload = session.current;
      if (!upload) throw new Error("上传会话未建立。");
      if (!upload.transferred) { setStage("正在传输源码包，请勿关闭页面…"); await platform.uploadSource(upload.transferPath, file, abort.signal); upload.transferred = true; }
      setStage("正在校验归档与 Dockerfile…");
      const response = await platform.sources.completeSourceUpload({ workspaceId: app.workspaceId, applicationId: app.id, uploadId: upload.uploadId }, { signal: abort.signal });
      if (!response.snapshot) throw new Error("控制面未返回源码快照，请重试完成校验，不要重新上传。");
      setSnapshot(response.snapshot); setStage(""); session.current = undefined;
    } catch (error) { setStage(""); throw error; }
  }}>
    <div className="space-y-2"><Label htmlFor="source-file">tar.gz 源码包</Label><Input id="source-file" disabled={!!stage} type="file" accept=".tar.gz,.tgz,application/gzip" onChange={(event) => { setFile(event.target.files?.[0]); setSnapshot(undefined); session.current = undefined; }} /><p className="text-xs text-muted-foreground">浏览器只计算摘要并传输原包，不读取或执行源码。源码包不保存在浏览器存储。</p></div>{stage && <div className="space-y-2"><Notice><span className="flex items-center gap-2"><Upload className="size-4" />{stage}</span></Notice><Button type="button" variant="outline" size="sm" onClick={() => controller.current?.abort()}>停止等待</Button></div>}
  </DataForm></section>{snapshot && <section className="section space-y-5"><Notice tone={snapshot.state === "READY" ? "success" : "neutral"}><span className="flex items-center gap-2"><Check className="size-4" />源码快照状态：{snapshot.state}。尚未创建构建。</span></Notice>{snapshot.state === "READY" && can(app, "build:write") && <Button asChild><Link to="/w/$workspaceSlug/apps/$applicationId/builds/new" params={{ workspaceSlug: workspaceSlug, applicationId: app.id }} search={{ snapshotId: snapshot.id }}>使用此快照创建构建</Link></Button>}<dl className="metadata"><div><dt>快照 ID</dt><dd><code>{snapshot.id}</code></dd></div><div><dt>Dockerfile</dt><dd><code>{snapshot.dockerfile}</code></dd></div><div><dt>压缩 / 展开大小</dt><dd>{snapshot.compressedBytes.toString()} / {snapshot.expandedBytes.toString()} 字节</dd></div><div><dt>条目数量 / 到期</dt><dd>{snapshot.entryCount.toString()} · {formatTime(snapshot.expiresAt)}</dd></div><div className="sm:col-span-2"><dt>SHA-256</dt><dd><code>{snapshot.sha256}</code></dd></div></dl><p className="text-xs text-muted-foreground">条目预览最多展示前 100 项，不在浏览器执行归档内容。</p><DataTable label="源码包条目" rows={snapshot.entries.slice(0, 100)} columns={[{ id: "path", title: "路径", cell: (entry) => <code className="text-xs">{entry.path}{entry.directory ? "/" : ""}</code> }, { id: "size", title: "大小（字节）", cell: (entry) => entry.size.toString() }]} /></section>}<section className="section"><h3 className="section-title">查找已有源码快照</h3><p className="mb-5 text-sm text-muted-foreground">当前接口不提供快照列表，可以按已知快照 ID 查询。</p><SnapshotLookup /></section></div>;
}
function SnapshotLookup() {
  const app = useApplication(); const [snapshotId, setSnapshotId] = useState("");
  const query = useRPC(SourceService.method.getSourceSnapshot, { workspaceId: app.workspaceId, applicationId: app.id, snapshotId }, { enabled: !!snapshotId && can(app, "source:read") });
  return <div className="stack"><DataForm submitLabel="查询快照" disabled={!can(app, "source:read")} fields={[{ name: "id", label: "快照 ID", schema: z.string().trim().min(1, "请填写快照 ID") }]} onSubmit={async (value) => { setSnapshotId(value.id); if (value.id === snapshotId) { const result = await query.refetch(); if (result.error) throw new Error(errorMessage(result.error)); } }} />{snapshotId && <QueryState query={query}>{query.data?.snapshot && <div className="rounded-md bg-muted p-4 text-sm"><p>快照：<code>{query.data.snapshot.id}</code></p><p>状态：{query.data.snapshot.state} · Dockerfile：{query.data.snapshot.dockerfile}</p>{query.data.downloadPath?.startsWith("/transfer/") && <a className="text-link mt-2 inline-block" href={query.data.downloadPath} download>下载原始源码包</a>}</div>}</QueryState>}</div>;
}
