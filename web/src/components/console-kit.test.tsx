import { cleanup, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";
import { DataForm, Pager, SecretOnce, required, resourceName } from "./console-kit";
afterEach(cleanup);

describe("正式控制台基础组件", () => {
  it("应用创建表单仅接受有效名称，后端失败保留输入", async () => {
    const submit = vi.fn(async () => { throw new Error("当前空间已有同名应用"); });
    const user = userEvent.setup();
    render(<DataForm fields={[{ name: "name", label: "应用名称", schema: resourceName }]} submitLabel="创建并配置" onSubmit={submit} />);
    await user.click(screen.getByRole("button", { name: "创建并配置" }));
    expect(submit).not.toHaveBeenCalled();
    await user.type(screen.getByLabelText("应用名称"), "my-service");
    await user.click(screen.getByRole("button", { name: "创建并配置" }));
    expect(await screen.findByRole("alert")).toHaveProperty("textContent", "当前空间已有同名应用");
    expect((screen.getByLabelText("应用名称") as HTMLInputElement).value).toBe("my-service");
  });
  it("确认密码不一致时阻止提交", async () => {
    const submit = vi.fn(); const user = userEvent.setup();
    render(<DataForm fields={[{ name: "newPassword", label: "新密码", schema: required }, { name: "confirm", label: "确认新密码", schema: required }]} onSubmit={submit} />);
    await user.type(screen.getByLabelText("新密码"), "password-one");
    await user.type(screen.getByLabelText("确认新密码"), "password-two");
    await user.click(screen.getByRole("button", { name: "保存更改" }));
    expect(await screen.findByText("两次输入的密码不一致")).toBeTruthy();
    expect(submit).not.toHaveBeenCalled();
  });
  it("分页使用原始服务端cursor且末页不能前进", async () => {
    const page = vi.fn(); const user = userEvent.setup();
    const { rerender } = render(<Pager cursor="" nextCursor="server.cursor" onPage={page} />);
    await user.click(screen.getByRole("button", { name: "下一页" }));
    expect(page).toHaveBeenCalledWith("server.cursor");
    rerender(<Pager cursor="server.cursor" onPage={page} />);
    expect((screen.getByRole("button", { name: "下一页" }) as HTMLButtonElement).disabled).toBe(true);
    await user.click(screen.getByRole("button", { name: "返回首页" }));
    expect(page).toHaveBeenLastCalledWith("");
  });
  it("一次性明文关闭后不再渲染且不会写入浏览器存储", async () => {
    const storage = vi.spyOn(Storage.prototype, "setItem"); const dismiss = vi.fn(); const user = userEvent.setup();
    const { unmount } = render(<SecretOnce value="ym_pat_once" onDismiss={dismiss} />);
    expect(screen.getByText("ym_pat_once")).toBeTruthy();
    await user.click(screen.getByRole("button", { name: "已保存，关闭" }));
    expect(dismiss).toHaveBeenCalledOnce();
    unmount();
    expect(screen.queryByText("ym_pat_once")).toBeNull();
    expect(storage).not.toHaveBeenCalled();
    storage.mockRestore();
  });
});
