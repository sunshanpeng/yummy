import { useId, useMemo, useState, type ReactNode } from "react";
import { useForm } from "@tanstack/react-form";
import { createColumnHelper, tableFeatures, useTable } from "@tanstack/react-table";
import { z } from "zod";
import { AlertCircle, ArrowRight, Check, Copy, LoaderCircle } from "lucide-react";
import { Button } from "./ui/button";
import { Input } from "./ui/input";
import { Label } from "./ui/label";
import { Textarea } from "./ui/textarea";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle, DialogTrigger } from "./ui/dialog";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "./ui/table";
import { errorMessage } from "../lib/platform";

export function Notice({ children, tone = "neutral" }: { children: ReactNode; tone?: "neutral" | "error" | "success" | "warning" }) {
  return <div role={tone === "error" ? "alert" : "status"} className={`rounded-md border px-4 py-3 text-sm ${tone === "error" ? "border-destructive/20 bg-danger-surface text-destructive" : tone === "success" ? "border-success/20 bg-success-surface text-success" : tone === "warning" ? "border-warning/20 bg-warning-surface text-warning" : "bg-muted text-muted-foreground"}`}>{children}</div>;
}
export function Loading({ label = "正在加载…" }: { label?: string }) {
  return <div role="status" className="flex min-h-40 items-center justify-center gap-2 text-muted-foreground"><LoaderCircle className="size-4 animate-spin" aria-hidden="true" />{label}</div>;
}
export function QueryState({ query, children, empty }: { query: { isPending: boolean; error: unknown; refetch: () => unknown }; children: ReactNode; empty?: boolean }) {
  if (query.isPending) return <Loading />;
  if (query.error) return <div className="space-y-3"><Notice tone="error"><span className="flex items-start gap-2"><AlertCircle className="mt-0.5 size-4 shrink-0" />{errorMessage(query.error)}</span></Notice><Button variant="outline" onClick={() => void query.refetch()}>重新加载</Button></div>;
  if (empty) return <Notice>资源不存在或当前账户无权访问。</Notice>;
  return children;
}
export function Heading({ title, description, action }: { title: string; description?: string; action?: ReactNode }) {
  return <div className="page-heading"><div><h1 className="page-title">{title}</h1>{description && <p className="subtitle">{description}</p>}</div>{action}</div>;
}
export function Empty({ title, description, children }: { title: string; description: string; children?: ReactNode }) {
  return <div className="rounded-lg border border-dashed bg-card px-6 py-14 text-center"><h2 className="text-base font-semibold">{title}</h2><p className="mx-auto mt-2 max-w-md text-sm text-muted-foreground">{description}</p>{children && <div className="mt-5">{children}</div>}</div>;
}
export type FieldGroup = { title: string; description?: string; fields: FieldDefinition[] };
export type FieldDefinition = { name: string; label: string; value?: string; type?: string; hint?: string; autoComplete?: string; readOnly?: boolean; required?: boolean; options?: { value: string; label: string }[]; schema?: z.ZodType<string> };
export const required = z.string().trim().min(1, "请填写此项");
// 统一命名规范（2026-10-26 设计）：小写字母开头，最长 30 位，无连续 --，不以 - 结尾。
export const resourceName = required.regex(/^[a-z](?:-?[a-z0-9]){0,29}$/, "使用小写字母开头的最长 30 位小写字母、数字或中划线（无连续中划线，不以中划线结尾）");
export const passwordSchema = z.string().refine((v) => { const length = new TextEncoder().encode(v).length; return length >= 12 && length <= 128; }, "密码必须为 12～128 个字节");

// 所有业务表单共用 TanStack Form 生命周期，字段与提交校验由 Zod 执行。
export function DataForm({ fields, groups, submitLabel = "保存更改", onSubmit, children, disabled = false, successMessage, footer, onValuesChange }: { fields?: FieldDefinition[]; groups?: FieldGroup[]; submitLabel?: string; onSubmit: (values: Record<string, string>) => Promise<void>; children?: ReactNode; disabled?: boolean; successMessage?: string; footer?: ReactNode; onValuesChange?: () => void }) {
  const id = useId();
  const [error, setError] = useState("");
  const [success, setSuccess] = useState(false);
  const allFields = useMemo(() => groups?.length ? groups.flatMap((group) => group.fields) : fields ?? [], [fields, groups]);
  const values = useMemo(() => Object.fromEntries(allFields.map((f) => [f.name, f.value ?? ""])), [allFields]);
  const schema = z.record(z.string(), z.string()).superRefine((values, context) => {
    for (const field of allFields) {
      const result = (field.schema ?? z.string()).safeParse(values[field.name]);
      if (!result.success) for (const issue of result.error.issues) context.addIssue({ code: "custom", path: [field.name], message: issue.message });
    }
    if (values.confirm !== undefined && values.confirm !== (values.newPassword ?? values.password)) context.addIssue({ code: "custom", path: ["confirm"], message: "两次输入的密码不一致" });
  });
  const form = useForm({
    defaultValues: values,
    validators: { onSubmit: schema },
    onSubmit: async ({ value }) => {
      setError(""); setSuccess(false);
      try {
        await onSubmit(value);
        // 成功后擦除密码输入；普通业务输入保留，失败时所有输入均保留以便修正。
        for (const field of allFields) if (field.type === "password") form.setFieldValue(field.name, "");
        setSuccess(true);
      } catch (reason) { setError(errorMessage(reason)); }
    },
  });
  return <form className="space-y-5" noValidate onSubmit={(event) => {
    event.preventDefault();
    const element = event.currentTarget;
    if (form.state.isSubmitting) return;
    void form.handleSubmit().then(() => {
      if (form.state.isValid === false) {
        const firstInvalid = element.querySelector<HTMLElement>('[aria-invalid="true"]');
        firstInvalid?.focus();
      }
    });
  }}>
    {(() => {
      const renderField = (definition: FieldDefinition) => <form.Field key={definition.name} name={definition.name}>{(field) => {
        const invalid = field.state.meta.errors.length > 0;
        const fieldID = `${id}-${definition.name}`;
        const props = { id: fieldID, name: definition.name, value: field.state.value, disabled: disabled || definition.readOnly, onBlur: field.handleBlur, onChange: (event: { target: { value: string } }) => { field.handleChange(event.target.value); setSuccess(false); onValuesChange?.(); }, "aria-invalid": invalid, "aria-describedby": `${fieldID}-description`, "aria-required": definition.required === true };
        return <div className="space-y-2"><Label htmlFor={fieldID}>{definition.label}{definition.required && <span aria-hidden="true" className="ml-1 text-destructive">*</span>}{definition.required && <span className="sr-only">（必填）</span>}</Label>{definition.options ? <select {...props} className="w-full">{definition.options.map((option) => <option key={option.value} value={option.value}>{option.label}</option>)}</select> : definition.type === "textarea" ? <Textarea {...props} rows={3} /> : <Input {...props} type={definition.type ?? "text"} autoComplete={definition.autoComplete ?? "off"} />}
        <div id={`${fieldID}-description`} className={`text-xs ${invalid ? "text-destructive" : "text-muted-foreground"}`}>{invalid ? field.state.meta.errors.map((error) => typeof error === "object" && error && "message" in error ? String(error.message) : String(error)).join("；") : definition.hint}</div></div>;
      }}</form.Field>;
      const sections = groups?.length ? groups : fields?.length ? [{ title: "", fields }] : [];
      return sections.map((section, index) => <section key={section.title || index} className={index > 0 ? "border-t pt-6" : ""}>{section.title && <h3 className="font-semibold">{section.title}</h3>}{section.description && <p className="mb-4 mt-1 text-xs text-muted-foreground">{section.description}</p>}{(!section.title || section.description) && section.title ? null : null}<div className={section.title ? "mt-4 space-y-5" : "space-y-5"}>{section.fields.map(renderField)}</div></section>);
    })()}
    {children}{error && <Notice tone="error">{error}</Notice>}{success && successMessage && <Notice tone="success">{successMessage}</Notice>}
    <div className="flex flex-wrap items-center gap-3 pt-1"><form.Subscribe selector={(state) => state.isSubmitting}>{(pending) => <Button disabled={disabled || pending} type="submit">{pending ? <><LoaderCircle className="animate-spin" />正在提交…</> : submitLabel}</Button>}</form.Subscribe>{footer}</div>
  </form>;
}

export function ConfirmAction({ label, title, description, onConfirm, destructive = false, disabled = false }: { label: string; title?: string; description: string; onConfirm: () => Promise<void>; destructive?: boolean; disabled?: boolean }) {
  const [open, setOpen] = useState(false); const [error, setError] = useState(""); const [pending, setPending] = useState(false);
  return <Dialog open={open} onOpenChange={(next) => { if (!pending) { setOpen(next); setError(""); } }}><DialogTrigger asChild><Button disabled={disabled} variant={destructive ? "destructive" : "outline"} size="sm">{label}</Button></DialogTrigger><DialogContent className="max-h-[90dvh] w-[calc(100%-2rem)] overflow-y-auto"><DialogHeader><DialogTitle>{title ?? label}</DialogTitle><DialogDescription>{description}</DialogDescription></DialogHeader>{error && <Notice tone="error">{error}</Notice>}<DialogFooter><Button variant="outline" disabled={pending} onClick={() => setOpen(false)}>取消</Button><Button variant={destructive ? "destructive" : "default"} disabled={pending} onClick={async () => { setPending(true); setError(""); try { await onConfirm(); setOpen(false); } catch (reason) { setError(errorMessage(reason)); } finally { setPending(false); } }}>{pending ? "正在处理…" : "确认"}</Button></DialogFooter></DialogContent></Dialog>;
}
export function SecretOnce({ value, title = "请立即保存", onDismiss }: { value: string; title?: string; onDismiss: () => void }) {
  const [copied, setCopied] = useState(false); const [copyError, setCopyError] = useState(false);
  return <section className="space-y-3 rounded-lg border border-warning/30 bg-warning-surface p-5" aria-label={title}><h2 className="font-semibold">{title}</h2><p className="text-sm text-warning">此内容只显示一次，离开或关闭后无法再次查看。请通过安全渠道保存或分享。</p><code className="block break-all rounded border bg-card p-3 text-sm">{value}</code><div className="flex flex-wrap gap-2"><Button variant="outline" size="sm" onClick={async () => { try { await navigator.clipboard.writeText(value); setCopied(true); } catch { setCopyError(true); } }}>{copied ? <Check /> : <Copy />}{copied ? "已复制" : "复制"}</Button><Button variant="ghost" size="sm" onClick={onDismiss}>已保存，关闭</Button></div>{copyError && <p role="alert" className="text-sm text-destructive">无法访问剪贴板，请手动选中复制。</p>}</section>;
}
export function Pager({ cursor, nextCursor, onPage }: { cursor: string; nextCursor?: string; onPage: (cursor: string) => void }) {
  return <div className="mt-5 flex flex-wrap items-center justify-between gap-3 text-xs text-muted-foreground"><span>按服务端游标分页，每页最多 24 条</span><div className="flex gap-2">{cursor && <Button variant="outline" size="sm" onClick={() => onPage("")}>返回首页</Button>}<Button variant="outline" size="sm" disabled={!nextCursor} onClick={() => nextCursor && onPage(nextCursor)}>下一页<ArrowRight /></Button></div></div>;
}
const features = tableFeatures({});
export type DisplayColumn<T> = { id: string; title: string; cell: (row: T) => ReactNode };
export function DataTable<T extends object>({ rows, columns, label }: { rows: T[]; columns: DisplayColumn<T>[]; label: string }) {
  const definitions = useMemo(() => {
    const helper = createColumnHelper<typeof features, T>();
    return helper.columns(columns.map((column) => helper.display({ id: column.id, header: column.title, cell: ({ row }) => column.cell(row.original) })));
  }, [columns]);
  const table = useTable({ features, columns: definitions, data: rows });
  return <div className="overflow-hidden rounded-lg border bg-card"><Table aria-label={label}><TableHeader>{table.getHeaderGroups().map((group) => <TableRow key={group.id}>{group.headers.map((header) => <TableHead key={header.id} className="whitespace-nowrap px-4"><table.FlexRender header={header} /></TableHead>)}</TableRow>)}</TableHeader><TableBody>{table.getRowModel().rows.map((row) => <TableRow key={row.id}>{row.getAllCells().map((cell) => <TableCell className="px-4 py-4" key={cell.id}><table.FlexRender cell={cell} /></TableCell>)}</TableRow>)}</TableBody></Table></div>;
}
