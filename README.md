# livekit-server

AV Platform 的业务服务仓库。

已实现 Go + SQLite 远控业务服务 `cmd/business-api`：临时配对、双端凭证、本地同意后的授权租约、媒体 Token、撤销与审计。构建与部署见 [远控运行文档](../docs/remote-control/README.md)，HTTP 契约见 [OpenAPI](api/openapi.json)。账号、会议、任务等其他规划能力尚未实现。

## 设计文档

- [业务服务设计](docs/design.md)：领域模型、鉴权、会议、控制授权、任务、资产和可靠性。
- [平台总体设计](../docs/platform-design.md)：在 AV Platform 同级仓库布局下阅读。

## 规划职责

- 账号、设备关系、会议与远控授权。
- 校验业务权限并签发短期媒体访问 Token。
- 媒体资产、计费与相关业务状态管理。
- 通过服务端 SDK 或管理 API 协调媒体服务。

媒体服务 API Secret 仅保存在可信后端，不下发给客户端。
本仓库负责业务服务；SFU 转发、TURN、录制和转码等媒体服务能力由 `livekit-cpp` 及相关服务承担。

## 关联仓库

- [av-platform](https://github.com/soundex-dele/av-platform)：聚合仓库与平台架构文档。
- [livekit-app](../livekit-app/README.md)：本地客户端仓库，尚未创建远程。
- [livekit-sdk-cpp](https://github.com/soundex-dele/livekit-sdk-cpp)：现有原生 C++ RTC SDK 及服务端 API 客户端基础。
- [livekit-cpp](https://github.com/soundex-dele/livekit-cpp)：媒体服务。
