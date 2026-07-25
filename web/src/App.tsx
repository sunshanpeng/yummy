import { FormEvent, useEffect, useRef, useState } from "react";

import { CommandStatus } from "./gen/demo/v1/demo_pb";

export interface WorkerView {
  id: string;
  version: string;
  lastSeen: string;
}

export interface CommandView {
  id: string;
  workerId: string;
  input: string;
  output: string;
  error: string;
  status: CommandStatus;
}

export interface ControlApi {
  listWorkers(): Promise<WorkerView[]>;
  createCommand(workerId: string, input: string): Promise<CommandView>;
  getCommand(id: string): Promise<CommandView>;
}

const terminalStatuses = new Set([
  CommandStatus.SUCCEEDED,
  CommandStatus.FAILED,
  CommandStatus.DISPATCH_FAILED,
]);

const statusLabels: Record<CommandStatus, string> = {
  [CommandStatus.UNSPECIFIED]: "未知",
  [CommandStatus.CREATED]: "已创建",
  [CommandStatus.DISPATCHED]: "已下发",
  [CommandStatus.RUNNING]: "执行中",
  [CommandStatus.SUCCEEDED]: "已成功",
  [CommandStatus.FAILED]: "执行失败",
  [CommandStatus.DISPATCH_FAILED]: "下发失败",
};

export function App({ api }: { api: ControlApi }) {
  const [workers, setWorkers] = useState<WorkerView[]>([]);
  const [workerID, setWorkerID] = useState("");
  const [input, setInput] = useState("");
  const [command, setCommand] = useState<CommandView>();
  const [loadingWorkers, setLoadingWorkers] = useState(true);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");
  const pollingToken = useRef(0);

  useEffect(() => {
    let active = true;
    api
      .listWorkers()
      .then((items) => {
        if (!active) return;
        setWorkers(items);
        setWorkerID((current) => current || items[0]?.id || "");
      })
      .catch((reason: unknown) => {
        if (active) setError(errorMessage(reason));
      })
      .finally(() => {
        if (active) setLoadingWorkers(false);
      });
    return () => {
      active = false;
      pollingToken.current += 1;
    };
  }, [api]);

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!workerID || !input.trim()) return;

    const token = pollingToken.current + 1;
    pollingToken.current = token;
    setSubmitting(true);
    setError("");
    setCommand(undefined);
    try {
      const created = await api.createCommand(workerID, input.trim());
      setCommand(created);
      setInput("");
      if (!terminalStatuses.has(created.status)) {
        void pollCommand(created.id, token);
      }
    } catch (reason) {
      setError(errorMessage(reason));
    } finally {
      setSubmitting(false);
    }
  }

  async function pollCommand(id: string, token: number): Promise<void> {
    await wait(500);
    if (pollingToken.current !== token) return;
    try {
      const current = await api.getCommand(id);
      if (pollingToken.current !== token) return;
      setCommand(current);
      if (!terminalStatuses.has(current.status)) {
        await pollCommand(id, token);
      }
    } catch (reason) {
      if (pollingToken.current === token) setError(errorMessage(reason));
    }
  }

  return (
    <main className="shell">
      <header className="hero">
        <div>
          <p className="eyebrow">MULTI-TOPOLOGY PLATFORM</p>
          <h1>一份代码，按需组合部署</h1>
          <p className="lede">
            React 控制台与 Go 控制面可以合并为单个二进制，也可以拆成控制面和远程 worker。
          </p>
        </div>
        <div className="connection-state">
          <span className="pulse" aria-hidden="true" />
          {loadingWorkers ? "正在连接控制面" : `${workers.length} 个 worker 在线`}
        </div>
      </header>

      <section className="topology-strip" aria-label="支持的部署拓扑">
        <article>
          <span>01</span>
          <strong>单进程</strong>
          <small>platform（默认 all）</small>
        </article>
        <article>
          <span>02</span>
          <strong>前后端分离</strong>
          <small>Vite + control</small>
        </article>
        <article>
          <span>03</span>
          <strong>节点分离</strong>
          <small>control + worker</small>
        </article>
      </section>

      <section className="workspace">
        <form className="command-panel" onSubmit={submit}>
          <div className="section-heading">
            <div>
              <p className="section-kicker">COMMAND</p>
              <h2>下发 uppercase 任务</h2>
            </div>
            <span className="safe-tag">安全内置任务</span>
          </div>

          <label htmlFor="worker">执行节点</label>
          <select
            id="worker"
            value={workerID}
            onChange={(event) => setWorkerID(event.target.value)}
            disabled={loadingWorkers || workers.length === 0}
          >
            {workers.length === 0 ? (
              <option value="">暂无在线 worker</option>
            ) : (
              workers.map((worker) => (
                <option key={worker.id} value={worker.id}>
                  {worker.id} · {worker.version || "unknown"}
                </option>
              ))
            )}
          </select>

          <label htmlFor="task-input">任务文本</label>
          <textarea
            id="task-input"
            value={input}
            onChange={(event) => setInput(event.target.value)}
            placeholder="例如：hello from customer site"
            rows={5}
          />

          <button type="submit" disabled={!workerID || !input.trim() || submitting}>
            {submitting ? "正在下发…" : "下发任务"}
          </button>
        </form>

        <section className="result-panel" aria-live="polite">
          <div className="section-heading">
            <div>
              <p className="section-kicker">LATEST RUN</p>
              <h2>执行结果</h2>
            </div>
            {command && (
              <span className={`status status-${command.status}`}>
                {statusLabels[command.status]}
              </span>
            )}
          </div>

          {error ? (
            <div className="error-box">{error}</div>
          ) : command ? (
            <div className="result-content">
              <dl>
                <div>
                  <dt>指令 ID</dt>
                  <dd>{command.id}</dd>
                </div>
                <div>
                  <dt>执行节点</dt>
                  <dd>{command.workerId}</dd>
                </div>
              </dl>
              <div className="output-block">
                <span>OUTPUT</span>
                <strong>{command.output || "等待 worker 回传…"}</strong>
              </div>
              {command.error && <div className="error-box">{command.error}</div>}
            </div>
          ) : (
            <div className="empty-state">
              <span>→</span>
              <p>选择在线 worker 并提交文本，这里会实时显示任务状态。</p>
            </div>
          )}
        </section>
      </section>
    </main>
  );
}

function wait(milliseconds: number) {
  return new Promise((resolve) => window.setTimeout(resolve, milliseconds));
}

function errorMessage(reason: unknown) {
  return reason instanceof Error ? reason.message : String(reason);
}
