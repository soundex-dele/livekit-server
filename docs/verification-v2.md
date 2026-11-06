# AV 控制面重构验证记录

验证日期：2026-11-06。本文记录本次实现已完成的自动化检查，配合[架构说明](architecture-v2.md)和[运行说明](operations-v2.md)阅读。

## 已完成的检查

| 层次 | 结果与范围 |
| --- | --- |
| 业务服务 | SQLite、PostgreSQL 18 业务用例通过，覆盖组织隔离、入会与授权、操作状态、四业务生命周期；业务包 `go vet` 通过 |
| Foundation 与控制面存储 | Foundation 全量测试及 PostgreSQL 用例通过；控制面 PostgreSQL、Redis 集成检查通过 |
| 合并验证 | 两个 Go 模块的全量测试与 `go vet`、Foundation 源码锁、合并 OpenAPI 生成检查通过 |
| 部署 | Docker Compose 配置检查、Linux 镜像构建，以及容器内迁移、API 启动、就绪探测与能力查询通过；容器冒烟使用 SQLite + memory |
| SFU | CTest 12/12 通过；媒体准入、权限限制及撤权等服务端自动化检查通过 |
| Windows 原生客户端 | `av_remote_host`、`av_screen_cast`、`livekit_conference_demo`、`av_live_studio` 编译成功 |
| 原生客户端测试 | `studio_http`、`remote_lifecycle`、`screen_cast_session`、`conference_widgets`，4/4 通过 |
| Web 客户端 | 投屏、远控、直播均通过 TypeScript/Vite 构建；测试分别 2、8、6 项通过，共 16 项 |
| 代码检查 | 客户端 `git diff --check` 通过；新增原生共享控制代码通过 clang-format 20.1.8 检查 |

客户端测试覆盖 Foundation Cookie/Bearer 适配、组织与资源凭据隔离、组织切换取消旧请求、媒体授权准备重试、取消后的资源释放，以及会议登录创建流程。原生 Qt 测试使用 `QT_QPA_PLATFORM=offscreen`，未操作用户桌面。

SFU 的 12 项为 `av-control`、`twirp`、`config`、`data-packets`、`join-request`、`subscription-permissions`、`rpc`、`media-compat`、`turn-credentials`、`native-send`、`native-rtp`、`core`，运行日志位于 `livekit-cpp/out/windows-x64/Testing/Temporary/LastTest.log`。

SFU HTTP/HTTPS 准入及信令合同脚本另已通过，覆盖管理凭据与 RTC 凭据隔离、节点启动代次和绑定版本隔离、重复及迟到命令、刷新令牌签发方与 60 秒 TTL、恢复连接的版本校验及周期撤权。

复核期间修复了 Foundation Worker 在领取/完成任务时收到取消信号的退出竞态，相关定向测试连续30次通过。源码锁在验证开始与结束分别检查，覆盖相邻 Foundation 目录的实际源码；并行修改导致的漂移必须复核后显式更新，不能仅凭验证开始时的匹配认定构建版本一致。

## 真实进程与合成媒体

结果文件为 [`out/control-plane-integration.json`](../out/control-plane-integration.json)，由 [`tests/control_plane_integration.py`](../tests/control_plane_integration.py) 生成。该文件属于运行产物；清理构建目录后需重新执行测试生成。

已通过的编排使用 **两个 API 进程、独立 Worker、真实 SFU 和独立 Redis 测试服务**。数据库是两个 API 共享的临时 SQLite 文件；PostgreSQL 已由单独的存储和业务测试覆盖，本次多进程媒体编排没有使用 PostgreSQL。

编排验证了跨 API 登录凭据复用、组织成员邀请、投屏码及 WebSocket 票据只消费一次、持久事件补拉、API 重启保留资源与房间绑定、结束后的旧媒体 JWT 拒绝及 SFU 房间清理。远控在持续发送在线心跳的情况下仍会因独立租约过期而失效；重新本地批准产生新 epoch。会议验证等候室准入和发布权限变更撤销旧授权。

无窗口浏览器使用 Canvas VP8 视频和振荡器 Opus 音频，取得真实 Go 服务签发的授权并连接真实 SFU。结果中的 `passed`、`real_sfu`、`real_rtc_signaling`、`media_packets` 均为 `true`。

| 场景 | 已观察到的结果 |
| --- | --- |
| 投屏 | 视频解码帧数与接收字节持续增加，收到非静音屏幕音频；停止后双方断开 |
| 会议 | 等候室生效；主持人与成员双向接收并解码音视频；结束后双方断开 |
| 直播 | program 音视频接收、独立 stage 双向音视频通过；关播后四个参与连接断开 |
| 有人值守远控 | 本地批准后接收屏幕视频；合成输入记录及回显通过；撤销后双方断开；未注入操作系统输入 |

四个场景均在原媒体 Token 到期前完成撤权：再次换取凭据返回 403，SFU 验证旧 Token 返回 401，相关媒体连接断开。这是功能正确性观察，不作为撤权时延或容量承诺。

最终编排还验证结束响应的 `accepted` / `operation_ids`、操作状态查询及跨组织拒绝。浏览器和本次启动的服务进程已退出，报告中 `process_cleanup_errors: []`、`temporary_directory_removed: true`；浏览器同时验证真实进程退出与驱动清理完成，避免仅凭媒体断言通过掩盖清理超时。

## 可复现命令

以下为本次已使用的客户端命令，基于现有 Windows 构建目录与依赖配置。首次配置参数见各客户端构建文档。

```powershell
Set-Location D:/workspace/av-platform/livekit-app
& 'D:/Program Files/CMake/bin/cmake.exe' --build out/rtc-products-qt6 --config Release --target av_http av_remote_host av_screen_cast livekit_conference_demo --parallel 2
& 'D:/Program Files/CMake/bin/cmake.exe' --build out/rtc-products-qt6 --config Release --target av_live_studio --parallel 2
& 'D:/Program Files/CMake/bin/cmake.exe' --build out/rtc-products-qt6 --config Release --target studio_http_tests av_remote_host livekit_conference_demo livekit_demo_tests remote_lifecycle_test screen_cast_session_test --parallel 2
& 'D:/Program Files/CMake/bin/ctest.exe' --test-dir out/rtc-products-qt6 -C Release -R '^studio_http$' --output-on-failure
& 'D:/Program Files/CMake/bin/ctest.exe' --test-dir out/rtc-products-qt6 -C Release -R '^(conference_widgets|remote_lifecycle|screen_cast_session)$' --output-on-failure

Set-Location D:/workspace/av-platform/livekit-app/apps/screen-cast/web
npm.cmd run build
npm.cmd test
Set-Location D:/workspace/av-platform/livekit-app/apps/remote-control/web
npm.cmd run build
npm.cmd test
Set-Location D:/workspace/av-platform/livekit-app/apps/live-studio/web
npm.cmd run build
npm.cmd test
```

后端全套检查、业务 PostgreSQL 定向测试和真实进程编排的已用入口如下。先在环境中配置独立测试服务的 `AV_TEST_PG_DSN`、`AV_TEST_REDIS_URL`，Foundation PostgreSQL 用例另需 `PF_TEST_PG_DSN`；不在报告内保存连接口令。编排需要已构建的业务服务、SFU 和浏览器测试依赖。主动更新合并契约时为 `verify.py` 添加 `--write-contract`，普通校验会拒绝契约漂移。

```powershell
Set-Location D:/workspace/av-platform/livekit-server
python tools/verify.py --go ../out/tools/go/bin/go.exe
& ../out/tools/go/bin/go.exe test ./internal/businesses/v2 -count=1 -timeout=120s
python tests/control_plane_integration.py --media-script ../livekit-cpp/tests/av_media_control_smoke.mjs
```

SFU 的已用命令：

```powershell
Set-Location D:/workspace/av-platform/livekit-cpp
cmake --build out/windows-x64 --config Release --parallel 3
ctest --test-dir out/windows-x64 -C Release --output-on-failure
python tests/av_control_contract.py --binary out/windows-x64/Release/livekit-server.exe
python tests/av_control_contract.py --binary out/windows-x64/Release/livekit-server.exe --go ../out/tools/go/bin/go.exe --tls
node tests/av_signal_contract.mjs --binary out/windows-x64/Release/livekit-server.exe
```

## 验证边界

- 没有执行真实摄像头、麦克风、扬声器、显示器采集或操作系统远控输入的人工验收；合成媒体通过不等于设备验收通过。
- 没有执行 macOS 构建或设备验证，也未完成高并发、长时间稳定性、广域网、TURN 或跨节点故障恢复的容量与性能验收。
- 部分旧 Web `integration.mjs`、UI fixture 和直播 publisher fixture 仍依赖旧服务启动方式或缺少组织预置，尚未迁移；未将这些脚本列为通过。新的四业务进程编排独立于这些旧脚本。
- 当前记录证明已列出的功能与自动化检查结果，不代表所有原生产品界面的端到端路径均已通过真实服务验收。
