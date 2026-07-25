import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { App, type CommandView, type ControlApi } from "./App";
import { CommandStatus } from "./gen/demo/v1/demo_pb";

describe("App", () => {
  it("选择 worker 后提交任务并显示结果", async () => {
    const command: CommandView = {
      id: "command-1",
      workerId: "worker-1",
      input: "hello",
      output: "HELLO",
      error: "",
      status: CommandStatus.SUCCEEDED,
    };
    const api: ControlApi = {
      listWorkers: async () => [{ id: "worker-1", version: "demo", lastSeen: "" }],
      createCommand: async () => ({
        ...command,
        output: "",
        status: CommandStatus.DISPATCHED,
      }),
      getCommand: async () => command,
    };
    const user = userEvent.setup();

    render(<App api={api} />);
    await screen.findByRole("option", { name: /worker-1/ });
    await user.type(screen.getByLabelText("任务文本"), "hello");
    await user.click(screen.getByRole("button", { name: "下发任务" }));

    expect(await screen.findByText("HELLO")).toBeTruthy();
    expect(screen.getByText("已成功")).toBeTruthy();
  });
});
