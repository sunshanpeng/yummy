import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-web";

import type { CommandView, ControlApi } from "./App";
import {
  ControlService,
  type Command,
} from "./gen/demo/v1/demo_pb";

const client = createClient(
  ControlService,
  createConnectTransport({ baseUrl: "/api" }),
);

export const controlApi: ControlApi = {
  async listWorkers() {
    const response = await client.listWorkers({});
    return response.workers.map((worker) => ({
      id: worker.id,
      version: worker.version,
      lastSeen: worker.lastSeen,
    }));
  },

  async createCommand(workerId, input) {
    const response = await client.createCommand({ workerId, input });
    if (!response.command) throw new Error("控制面没有返回指令");
    return commandView(response.command);
  },

  async getCommand(id) {
    const response = await client.getCommand({ id });
    if (!response.command) throw new Error("控制面没有返回指令");
    return commandView(response.command);
  },
};

function commandView(command: Command): CommandView {
  return {
    id: command.id,
    workerId: command.workerId,
    input: command.input,
    output: command.output,
    error: command.error,
    status: command.status,
  };
}
