# 本仓库定制项（fork 维护说明）

> 本文件列出本 fork 相对上游（Wox-launcher/Wox）的全部定制改动，便于后续合并上游时定位冲突。
> 每次同步上游后请更新本文件：已完成合并的条目保留并标注，新增定制继续追加。
> 注意：上游仓库不含此文件与本文件涉及的改动，合并时以下文件均可能冲突。

## 1. 单击打开搜索结果（行为变更，无新增设置项）

- 列表行、网格视图、title-tag 徽标、tail 标签的单击手势改为 select + activate 一步执行；`OnDoubleTap` 注册为空操作以吞掉双击的第二击，防止结果执行两次。
- 涉及文件：
  - `wox.core/ui/launcher/view/launcher_results_view.go`
  - `wox.core/ui/launcher/view/launcher_grid_view.go`
  - `wox.core/ui/launcher/view/launcher_results_view_test.go`（新增单击/双击/嵌套热点测试）
  - `wox.core/ui/launcher/view/launcher_grid_view_test.go`（新增单击接线测试）

## 2. Space Quick Look 增强（仅 Windows，短按切换 / 长按窥视 / Esc 关闭）

- `selection.go` 状态机重写：短按（<200ms）切换预览开关；长按（≥200ms）松手即关（窥视）；Escape 关闭；打开失败自动回退；keyup 早于打开完成的竞态用"期望终态 + 打开完成后补关闭"与 FIFO 串行队列处理。
- 预览窗口不抢 Explorer 焦点：新增 `ShowWithoutActivation`（公共契约字段）→ `contract.ShowOptions` → `WindowOptions.NoActivateOnShow`（Windows 用 `SW_SHOWNOACTIVATE` 展示，点击仍可正常激活；刻意未用 `Nonactivating`，因其会禁用 WebView 嵌入层且点击无法激活）。
- 新增 `CloseWoxInstance(ctx, instanceName)` API：`common.UI` 接口 → `ui_impl.go` → `contract.View.CloseInstance` → launcher 实例注册表 `closeNamed`。
- 预览窗自身键盘兜底：preview-only 实例内按空格关闭（`launcherPreviewOnlySpaceClose`）。
- 预览窗口居中于 Explorer / 打开保存对话框矩形上方。
- 涉及文件：
  - `wox.core/plugin/system/selection.go`
  - `wox.core/plugin/system/selection_space_quicklook_test.go`（新增状态机单测）
  - `wox.core/common/ui.go`（`ShowContext.ShowWithoutActivation`、`UI.CloseWoxInstance`）
  - `wox.core/ui/contract/view.go`（`ShowOptions.ShowWithoutActivation`、`View.CloseInstance`）
  - `wox.core/ui/ui_impl.go`
  - `wox.core/ui/launcher/instances.go`
  - `wox.core/ui/launcher/app.go`
  - `wox.core/ui/launcher/app_test.go`
  - `wox.core/ui/runtime/internal/window/window.go`（`WindowOptions.NoActivateOnShow`）
  - `wox.core/ui/runtime/internal/window/window_windows.go`（passive show 路径）
  - `ci/pluginhealth/pluginhealth.go`（`CloseWoxInstance` 空实现桩）

## 3. CI：只编译 my-feature 分支且只编译 Windows

- `.github/workflows/build.yml`：push 触发分支改为 `my-feature`（另保留 `workflow_dispatch`）；仅启用 `build_windows_amd64`；关闭 macOS 公证与 Windows SignPath 签名（fork 无对应 secrets）。`build-core.yml` 未改动。
- 注意：上游若重构 `build.yml` / `build-core.yml`，此文件几乎必然冲突，合并时保留本 fork 版本或重新套用三点改动即可。

## 4. 自动更新默认关闭

- `wox.core/setting/wox_setting.go`：`EnableAutoUpdate` 默认值 `true` → `false`，避免自构建版本自动更新到上游 master 发布通道。
- 注意：仅影响新装/未显式保存过该设置的环境；已有配置中保存过 `true` 的仍需在设置中手动关闭。
