import { useId, useState } from "react";
import { parseEnv, type EnvEntry } from "../lib/env-import";
import { Notice } from "./console-kit";
import { Button } from "./ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "./ui/dialog";
import { Input } from "./ui/input";
import { Label } from "./ui/label";
import { Textarea } from "./ui/textarea";

const maxImportBytes = 2 * 1024 * 1024;

export function EnvImport({ variables, onApply }: {
  variables: { name: string; value: string; sensitive: boolean }[];
  onApply: (entries: EnvEntry[]) => void;
}) {
  const id = useId();
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [text, setText] = useState("");
  const [error, setError] = useState("");
  const [preview, setPreview] = useState<EnvEntry[]>();
  const existing = new Map(variables.map((variable) => [variable.name, variable]));
  const pending = preview?.filter(({ name, value }) => {
    const current = existing.get(name);
    return !current || (!current.sensitive && current.value !== value);
  }) ?? [];

  function close(next: boolean) {
    if (loading) return;
    setOpen(next);
    if (!next) { setText(""); setPreview(undefined); setError(""); }
  }

  function inspect() {
    try {
      if (new TextEncoder().encode(text).length > maxImportBytes) throw new Error("导入内容最多 2 MiB。");
      const entries = parseEnv(text);
      if (!entries.length) throw new Error("没有可导入的变量。");
      const encoder = new TextEncoder();
      if (entries.some(({ value }) => value.includes("\0") || encoder.encode(value).length > 65536)) {
        throw new Error("变量值不能包含空字符，单个值最多 64 KiB。");
      }
      if (variables.length + entries.filter(({ name }) => !existing.has(name)).length > 200) {
        throw new Error("导入后环境变量不能超过 200 项。");
      }
      setError("");
      setPreview(entries);
    } catch (reason) {
      setError(reason instanceof Error ? reason.message : "无法读取导入内容。");
    }
  }

  return <Dialog open={open} onOpenChange={close}>
    <DialogTrigger asChild><Button type="button" variant="outline" size="sm">导入 .env</Button></DialogTrigger>
    <DialogContent className="max-h-[90dvh] w-[calc(100%-2rem)] overflow-y-auto">
      <DialogHeader>
        <DialogTitle>导入运行变量</DialogTitle>
        <DialogDescription>在本地预览变量名和变更类型。新增变量默认标为敏感，已有敏感值保留。</DialogDescription>
      </DialogHeader>
      {preview ? <ul aria-label="导入预览" className="max-h-64 divide-y overflow-y-auto rounded-md border text-sm">
        {preview.map(({ name, value }) => {
          const current = existing.get(name);
          return <li key={name} className="flex justify-between gap-4 p-3">
            <code className="break-all">{name}</code>
            <span className="shrink-0 text-muted-foreground">{!current ? "新增 · 敏感" : current.sensitive ? "保留敏感值" : current.value === value ? "相同" : "覆盖普通值"}</span>
          </li>;
        })}
      </ul> : <div className="space-y-3">
        <Label htmlFor={`${id}-file`}>选择文件（选填）</Label>
        <Input id={`${id}-file`} type="file" accept=".env,text/plain" disabled={loading} onChange={async (event) => {
          const file = event.target.files?.[0];
          if (!file) return;
          setLoading(true);
          setError("");
          try {
            if (file.size > maxImportBytes) throw new Error("导入文件最多 2 MiB。");
            setText(await file.text());
          } catch (reason) {
            setError(reason instanceof Error ? reason.message : "无法读取文件。");
          } finally {
            setLoading(false);
          }
        }} />
        <Label htmlFor={`${id}-text`}>.env 内容</Label>
        <Textarea id={`${id}-text`} value={text} rows={7} disabled={loading} onChange={(event) => setText(event.target.value)} spellCheck={false} autoComplete="off" />
      </div>}
      {error && <Notice tone="error">{error}</Notice>}
      <DialogFooter>
        <Button type="button" variant="outline" disabled={loading} onClick={() => preview ? setPreview(undefined) : close(false)}>{preview ? "返回编辑" : "取消"}</Button>
        {preview ? <Button type="button" disabled={!pending.length} onClick={() => { onApply(pending); close(false); }}>应用到表单</Button>
          : <Button type="button" disabled={loading} onClick={inspect}>预览导入</Button>}
      </DialogFooter>
    </DialogContent>
  </Dialog>;
}
