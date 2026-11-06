# 统一管理后台验证记录

日期：2026-11-06。实现与运行方式见[管理后台说明](admin.md)。本次记录仅覆盖管理后台新增功能；原有客户端验证见[控制面验证记录](verification-v2.md)。

## 服务与权限

`tools/verify.py` 已通过两个 Go 模块的全量测试、`go vet`、合并 OpenAPI 校验，以及验证开始和结束时的 Foundation 源码锁检查。测试使用独立 PostgreSQL 18 与 Redis 实例，同时保留 SQLite 覆盖。

新增用例覆盖：平台管理员治理未加入的组织、普通组织接口仍拒绝非成员、组织之间的隔离、资源凭据与 API Key 拒绝、管理 DTO 不泄漏凭据、四业务生命周期与成员治理、主持人保护、远控本地批准边界、直播 PK 收尾、稳定幂等响应、真实操作者审计、媒体任务关联，以及追加迁移保留既有校验和和数据。Foundation 的成员邀请、角色调整和移除在同一事务内写入平台与组织审计。

## 页面与公开包

后台的 OpenAPI 类型生成检查、TypeScript 检查、6 项单元测试和生产构建通过。单元测试包含媒体任务重试同时要求管理与写入权限的边界。5 项无窗口界面测试覆盖原因必填、治理请求与清理状态区分、失败重试复用幂等键、主持人移除限制、切换组织后丢弃迟到响应、组织管理员页面隔离，以及 1440 / 800 / 390 像素和深色布局。

Foundation 的 15 项 TypeScript 单元测试、四个公开 npm 包构建及隔离安装检查通过；4 项既有界面回归和 1 项真实非成员组织治理测试通过。管理后台只安装公开 tarball，源码及包完整性由 `foundation-web.lock.json` 固定。

跨系统归档复核发现 WSL 文件权限导致 npm 包摘要不同，已通过构建时统一公开文件权限修复。无网络 Linux 打包与 Windows 的四包归档逐字节一致；另用隔离的全 CRLF 源码副本构建，16 个产物（含 source map）与 LF 构建一致。包说明文档也纳入来源锁，未放宽 npm 完整性校验。

[`out/admin-ui-integration.json`](../out/admin-ui-integration.json) 的生产页面联调已通过：真实登录与平台概览、SFU 节点状态、非成员组织的成员／角色／审计／任务页面、会议搜索与详情、填写原因并结束真实会议，以及切回全部组织、深色主题和窄屏布局。浏览器异常与静态资源错误为空；API 查询确认会议实际进入 `ended`，不是仅改变界面显示。真实截图位于 `livekit-app/apps/admin/web/out/real-service`，已检查桌面、治理结果与 390 像素布局。

最终页面回归使用 Playwright 配套的 Chromium headless shell 141.0.7390.37，并开启系统“减少动态效果”偏好。该场景暴露并修复了全局压缩动画时长导致下拉层停留在屏幕外的问题：现在通过 Ant Design 的主题动效开关响应偏好。完整回归和浏览器清理均已通过，普通动效模式的真实服务对照也通过。临时 SQLite 的两个 API 按就绪顺序启动，避免初始化日志模式时争抢锁；测试阶段仍并发运行两个 API 与 Worker。

## 真实媒体治理

[`out/admin-media-integration.json`](../out/admin-media-integration.json) 记录两个 API、独立 Worker、真实 SFU、独立 Redis 和共享临时 SQLite 的运行结果。平台管理员没有加入受管组织，也没有进入媒体会话。

无窗口 Chromium headless shell 使用 Canvas VP8 视频与振荡器 Opus 音频，六项媒体断言全部通过：真实服务授权与准入、投屏音视频接收、管理员结束投屏、会议双向音视频与结束、直播 program / stage 音视频与关播、远控画面与合成输入回显及管理员撤销控制。

四场景均观察到原媒体 Token 到期前连接断开、再次换取凭据返回 403、SFU 验证旧 Token 返回 401。报告中 `passed`、`real_sfu`、`real_rtc_signaling`、`media_packets` 为 `true`，全部清理步骤成功，浏览器真实退出及驱动清理完成，`process_cleanup_errors` 为空，临时目录已移除。前一次普通 Chromium 的退出清理失败，改用独立 headless shell 后完整复测通过，未放宽媒体或清理断言。

## 容器交付

Compose 配置检查和最终 Linux 多阶段镜像构建通过。镜像 `av-admin-verify:20261106-admin` 内执行了严格来源锁／tarball 完整性校验、契约检查、类型检查、6 项单测及生产页面构建。受本机网络影响，验证构建使用 `GOPROXY=https://goproxy.cn,direct` 和 `NPM_CONFIG_REGISTRY=https://registry.npmmirror.com`，校验保持开启。

最终镜像的 22 项冒烟检查全部通过，包括非 root 运行、SQLite 临时库迁移与初始化、就绪探测、后台 HTML 和四个构建资源、能力入口、匿名请求拒绝、Cookie 登录、平台与组织管理查询、CSRF 防护。测试容器已移除；报告为聚合仓库的 [`out/admin-docker-smoke.json`](../../out/admin-docker-smoke.json)。该冒烟使用 SQLite 开发模式，生产 PostgreSQL / Redis 由上述独立后端测试覆盖；没有执行生产部署。

## 复现与边界

命令见[管理后台自动化验证](admin.md#自动化验证)。测试报告和截图属于运行产物，清理 `out` 后需重新生成。可通过 `LK_CHROME` 和 `AV_CHROME_PATH` 分别指定媒体与管理页面测试的 Chromium headless shell。

本次未操作用户桌面；未完成真实摄像头、麦克风、扬声器、桌面采集和操作系统远控输入的人工验收。合成媒体、页面测试与容器冒烟不代表实际设备、生产部署、高并发、广域网或长期稳定性验收。
