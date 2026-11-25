import { describe, expect, it } from "vitest";
import { parseEnv } from "./env-import";

describe("导入运行变量", () => {
  it("解析注释、空值、引号内的井号和多行配置", () => {
    expect(parseEnv('\uFEFF# 本地变量\r\nexport EMPTY=\r\nURL=https://example.test?a=b # 注释\r\nKEY="a#b\nc"\r\nRAW=\'  spaces  \'\r\n')).toEqual([
      { name: "EMPTY", value: "" },
      { name: "URL", value: "https://example.test?a=b" },
      { name: "KEY", value: "a#b\nc" },
      { name: "RAW", value: "  spaces  " },
    ]);
  });

  it("拒绝重复键或格式错误，错误中不回显输入值", () => {
    expect(() => parseEnv("KEY=one\nKEY=private-value")).toThrow("第 2 行：变量名称重复。");
    expect(() => parseEnv('TOKEN="private-value')).toThrow("第 1 行：引号未闭合。");
    expect(() => parseEnv("BAD-NAME=private-value")).toThrow("第 1 行：请输入合法的变量名和等号。");
  });
});
