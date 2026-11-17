// 平台健康探针路径：/healthz（对应 Node.js 服务模板）。
export const dynamic = "force-dynamic";
export function GET() {
  return Response.json({ status: "ok" });
}
