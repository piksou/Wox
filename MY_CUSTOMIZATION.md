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

## 3. CI：只编译 my-feature 分支且只编译 Windows（build + release）

- `.github/workflows/build.yml`：push 触发分支改为 `my-feature`（另保留 `workflow_dispatch`）；仅启用 `build_windows_amd64`；关闭 macOS 公证与 Windows SignPath 签名（fork 无对应 secrets）。`build-core.yml` 未改动。
- `.github/workflows/release.yml`：仅 tag（`v*`）触发；同样只构建 Windows、关闭签名/公证；checksums 按实际产物动态生成并附 `checksums.md5`；删除上游“向 master 推 updater.json/updater.beta.json”的步骤（本 fork 的 manifest 无人消费，且避免污染跟踪上游的 master 分支）。发布方式：`git tag v<版本> && git push origin v<版本>`。
- 注意：上游若重构 `build.yml` / `build-core.yml` / `release.yml`，这些文件几乎必然冲突，合并时保留本 fork 版本或重新套用上述改动即可。

## 4. 自动更新：仅检测并一次性提示，禁用实质更新动作

- 行为（`wox.core/updater/updater.go`）：
  - 更新检查始终执行（移除 `EnableAutoUpdate` 门控；本 fork 的检查无副作用，只为驱动提示）。
  - 发现上游新版本时不再自动下载：改为通过 `util/notifier` 提示一次“上游 Wox 发布了新版本，请自行合并上游更改并重新编译”。每个上游版本只提示一次，已提示的版本号记录在 Wox 数据目录 `updates/upstream_notified_version.txt`。
  - `ApplyUpdate` 顶部直接返回错误；`downloadUpdate` 与各平台更新实现保留但已无调用路径。自构建版本绝不自动下载/替换为官方安装包。
- 版本号纪律：`wox.core/updater/version.go` 的 `CURRENT_VERSION` 必须与所基于的上游 release 版本完全一致（当前 `2.4.8`），不得加 `-fork` 之类的后缀，否则 semver 比较无法正确检测官方新版本。同步上游时更新该常量，release tag 也使用同一版本号（如 `v2.4.8`）。
- `wox.core/setting/wox_setting.go`：`EnableAutoUpdate` 默认值 `true` → `false`（该设置现在只影响 update 插件 UI 展示，不再门控更新检查）。
- 合并上游注意：`updater.go` 的三处 fork 改动（门控移除、`downloadUpdate` 调用替换为 `notifyUpstreamReleaseOnce`、`ApplyUpdate` 守卫）需保留。
