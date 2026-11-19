# 前端组件与设计来源

## 官方组件源码

以下 Radix 系 shadcn/ui 组件按需从官方 registry 获取，父级下载至 `work/shadcn/` 后，本轮先读取 JSON，再提取 `files[].content` 入库：

- `button`、`input`、`label`、`dialog`、`dropdown-menu`、`table`、`badge`、`textarea`。
- 官方项目与文档：[shadcn/ui](https://ui.shadcn.com/)、[registry](https://ui.shadcn.com/docs/registry)。
- 官方 registry 路径：`https://ui.shadcn.com/r/styles/new-york/{组件名}.json`。
- 入库目录：`src/components/ui/`，不引入 React Hook Form。
- 本地适配：别名改为相对路径；Dialog 遮罩改用语义 `overlay` token；关闭文案中文；菜单指针与移动端确认按钮间距调整。未把普通业务页手写成替代 Radix 的菜单或焦点陷阱。

Lucide React 统一图标；TanStack Table 使用已安装 v9 的 `tableFeatures/createColumnHelper/useTable`，不复制 v8 `useReactTable` 示例。

## 设计边界

实施前加载全局 `ui-ux-pro-max` 与 `frontend-design`，执行中性浅色开发者控制台设计系统与 shadcn 表单/可访问性检索。建议中与项目冲突的彩色块、外链字体、React Hook Form 均未采用。

本版选择克制的工具型界面：中性浅色、细边界、14px 正文、明确留白与导航作用域。正式入口使用 `console.css`，色值仅定义在语义 token 中，不提供主题切换入口，不加载外部字体，不显示装饰图表或示例运行结果。

旧 uppercase 演示及原测试（`demo.html`、`demo-main.tsx`、`App.tsx`、`api.ts`、`styles.css`）已移除，原型阶段结束。正式入口为 `index.html`（`src/main.tsx`），样式仅 `src/console.css`，色值只定义在语义 token 中。
