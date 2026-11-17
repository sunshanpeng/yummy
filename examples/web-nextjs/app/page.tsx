export default function Home() {
  return (
    <main style={{ fontFamily: "system-ui", padding: 40 }}>
      <h1>Hello from Yummy PaaS</h1>
      <p>Next.js 独立部署示例。健康检查：<a href="/healthz">/healthz</a></p>
    </main>
  );
}
