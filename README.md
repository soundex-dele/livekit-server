# livekit-server

AV Platform 的业务服务仓库。

`cmd/business-api` 现使用 `platform-foundation` 装配账号、组织权限、事务、审计与持久任务，提供投屏、会议、有人值守远控和 WebRTC 直播。生产使用 PostgreSQL + Redis，API/Worker 可分进程部署；SFU 在 `livekit-cpp` 独立运行并启用 AV 准入模式。

新业务前缀为 `/api/v1/av`。本次采用新数据库和同步客户端升级；旧接口、测试数据不自动迁移。旧业务包保留作行为回归参照，不由新入口挂载。

- [当前架构](docs/architecture-v2.md)：模块、数据所有权、授权、媒体节点及故障语义。
- [运行与验证](docs/operations-v2.md)：构建、迁移、初始化、部署及验证入口。
- [验证记录](docs/verification-v2.md)：已通过的自动化检查、复现命令和设备验收边界。
- [统一运营管理后台](docs/admin.md)：构建与部署、平台／组织治理、四业务管理、媒体节点监控及无窗口验证。
- [合并契约](api/openapi.json)：Foundation与AV接口；`business-api openapi` 可重新生成。

## 设计文档

- [历史业务服务设计](docs/design.md)：旧版本设计背景，当前实现以新架构文档为准。
- [平台总体设计](../docs/architecture/platform-design.md)：在 AV Platform 同级仓库布局下阅读。

## 服务职责

- 复用 Foundation 账号、组织与权限，管理四类业务生命周期。
- 校验业务权限、签发短期媒体凭据并执行在线准入。
- 持久化业务状态、审计、事件及异步媒体操作。
- 通过带版本校验的管理 API 协调 SFU，管理有人值守远控租约。

媒体服务 API Secret 仅保存在可信后端，不下发给客户端。
本仓库负责业务服务；SFU 转发、TURN、录制和转码等媒体服务能力由 `livekit-cpp` 及相关服务承担。

## 关联仓库

- [av-platform](https://github.com/soundex-dele/av-platform)：聚合仓库与平台架构文档。
- [livekit-app](../livekit-app/README.md)：本地客户端仓库，尚未创建远程。
- [livekit-sdk-cpp](https://github.com/soundex-dele/livekit-sdk-cpp)：现有原生 C++ RTC SDK 及服务端 API 客户端基础。
- [livekit-cpp](https://github.com/soundex-dele/livekit-cpp)：媒体服务。

## 多业务边界

`internal/businesses/v2` 内四类用例拥有各自状态机和业务表，`internal/platform/control` 提供媒体准入、事务与实时机制，`bootstrap` 负责 Foundation 装配和传输。Web路径继续使用 `/apps/<业务>/`，通过 `AV_SCREEN_CAST_WEB`、`AV_REMOTE_CONTROL_WEB`、`AV_LIVE_STUDIO_WEB` 指定构建目录。

- 新运行命令：`migrate` → `bootstrap` → `serve`；独立进程使用 `api` / `worker`。默认启用四业务，可通过 `AV_MODULES` 关闭。
- 历史 `api/<业务>` 与旧设计文档用于追溯旧版本；当前契约以合并OpenAPI和新架构文档为准。
