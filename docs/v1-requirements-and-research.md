# 微信读书自动阅读工具 V1 需求与前期调研记录

## 1. 背景

计划自行实现一个轻量的微信读书自动阅读工具。

当前主要参考两个开源项目：

- wxread  
  https://github.com/findmover/wxread

- weread.koplugin  
  https://github.com/finlater/weread.koplugin

本项目不计划直接复制或简单 fork 任一项目。

当前思路是：

- 参考 `wxread` 的小范围、轻量自动阅读模型；
- 参考 `weread.koplugin` 对微信读书 Web Reader、阅读状态、签名和阅读时长上报协议更完整的研究；
- 重新实现一个范围更小、职责更清晰的 Go 工具。

本文记录当前需求、已经讨论并形成的产品决策，以及前期协议调查得到的结论。

协议相关内容属于当前调研结果，后续正式设计和实施前仍应独立复核，不应视为不可质疑的既定事实。


## 2. 项目目标

V1 的目标是一个：

- 单账号；
- 轻量；
- 无 Web UI；
- Docker 常驻部署；
- 可长期无人值守；
- 自动维护微信读书登录状态；
- 每日自动累计一定阅读时长；
- 运行时间和阅读时长具有适度随机性；
- 支持指定多个候选书籍，也支持自动选书；
- 支持 Bark 和企业微信机器人通知；

的小型服务。

主要部署环境包括：

- Apple Silicon Mac mini 等 ARM64 设备；
- 常见 x86_64 Linux 主机、VPS、NAS 等。


## 3. 技术栈

### 3.1 语言

使用 Go。

选择 Go 的主要原因：

- 项目本身以 HTTP 请求、状态管理和定时任务为主，适合 Go；
- 可以借项目学习和实践 Go；
- 可以编译为单一二进制；
- 容器运行环境简单；
- 容易生成 amd64 和 arm64 多架构镜像。


### 3.2 部署

使用 Docker。

GitHub Actions 自动：

1. 运行必要测试；
2. 构建 Docker 镜像；
3. 构建至少以下两个 Linux 架构：
   - `linux/amd64`
   - `linux/arm64`
4. 推送到镜像仓库，例如 GHCR。

部署端只需要拉取对应架构镜像并使用 Docker Compose 运行。

Docker 容器本身只运行 Go 程序。

不计划在容器中额外运行：

- cron daemon；
- supervisor；
- shell 定时脚本；
- `tail -f /dev/null` 等保活逻辑。


## 4. 参考项目分析

### 4.1 wxread

仓库：

https://github.com/findmover/wxread

`wxread` 是一个非常小的 Python 项目。

其主要组成包括：

- 微信读书 `/web/book/read` 阅读时长上报；
- Cookie renewal；
- 阅读请求签名；
- 简单异常处理；
- 多种通知渠道；
- GitHub Actions；
- Docker + cron。

真正和微信读书协议相关的核心代码主要集中在 `main.py`。

其基本运行模型是：

    已有 Cookie / 抓包数据
        ↓
    renewal
        ↓
    构造 /web/book/read
        ↓
    每约 30 秒上报
        ↓
    累计阅读时长

`wxread` 的工程范围非常接近本项目需要解决的问题，因此适合作为 V1 功能范围参考。

但其部分协议状态采用静态数据或兼容性 workaround，例如：

- 一部分 `/web/book/read` payload 使用预先准备的数据；
- `KEY` 使用固定 token；
- 书籍和章节来自代码中的静态候选集合；
- Cookie 更新主要围绕 `wr_skey`；
- 部分 `synckey` 异常处理依赖固定 `chapterInfos` 请求；
- Docker 中使用 cron。

这些内容不计划直接继承。


### 4.2 weread.koplugin

仓库：

https://github.com/finlater/weread.koplugin

这是一个功能范围明显更大的 KOReader 微信读书插件。

它涉及：

- 登录与认证；
- 书架；
- 书籍和章节信息；
- 内容读取；
- 阅读进度；
- 阅读时间上报；
- 划线、想法等功能；
- Reader/Web API 相关协议研究。

本项目不需要其绝大部分 KOReader 和内容阅读能力。

对本项目有价值的部分主要是：

- Web Reader 状态获取；
- book/chapter ID 编码；
- `/web/book/read` payload；
- `s` / `sg` 等签名；
- Reader Context；
- 阅读进度；
- Cookie renewal；
- 阅读时间上报状态机；
- 失败后的 Context/session 刷新方式；
- 书架获取和书籍选择相关能力。

因此，`weread.koplugin` 更适合作为协议研究参考，而不是产品范围参考。


## 5. 当前协议调研结论

以下内容是前期调查得到的当前认识，后续正式 workflow 应再次验证。


### 5.1 `/web/book/read`

阅读时间累计的核心接口为：

    POST /web/book/read

当前调查显示，真实 Web Reader 的请求包含以下一类数据：

- 书籍；
- 章节；
- 阅读位置；
- 阅读进度；
- Reader session/context；
- 本次活动时长；
- 时间戳；
- 随机值；
- 请求签名。


### 5.2 Reader Context

当前研究表明，打开微信读书 Web Reader 页面后，可以从页面中的：

    window.__INITIAL_STATE__

获得 Reader 相关状态。

目前已发现的重要字段包括：

- bookId；
- reader token；
- psvts；
- pclts；
- current chapter；
- progress 等。

本项目计划优先从真实 Reader Context 获取这些信息，而不是长期依赖一次抓包得到的静态 request template。


### 5.3 `reader.token` 与 wxread 的固定 KEY

`wxread` 当前存在固定值：

    3c5c8717f3daf09iop3423zafeqoi

并将其作为生成 `sg` 的 `KEY` 使用。

当前 `weread.koplugin` 的研究和实现显示，`sg` 更准确的模型是：

    SHA256(ts + rn + reader.token)

而且 Reader 页面可以提供动态的：

    reader.token

`weread.koplugin` 中也存在相同的：

    3c5c8717f3daf09iop3423zafeqoi

但其语义更接近默认或兼容 fallback token，而不是正常路径中必须硬编码的“盐”。

因此本项目计划：

- 正常路径使用 Reader Context 提供的 token；
- 不把固定值作为核心协议假设；
- 是否保留默认 token 作为 fallback，应在正式调查后决定。


### 5.4 请求中的主要字段

当前研究认为，下列字段大致具有以下来源：

| 字段 | 当前理解 |
| --- | --- |
| `appId` | 根据客户端/User-Agent 规则生成 |
| `b` | bookId 编码结果 |
| `c` | chapterUid 编码结果 |
| `ci` | 当前章节/阅读位置状态 |
| `co` | 当前章节 offset |
| `sm` | 当前阅读位置附近的摘要信息 |
| `pr` | 当前阅读进度 |
| `rt` | 本次实际阅读活动时间 |
| `ts` | 当前毫秒时间 |
| `rn` | 随机值 |
| `sg` | `ts + rn + reader.token` 的 SHA256 |
| `ct` | 当前客户端时间 |
| `ps` | Reader Context 中的 `psvts` |
| `pc` | Reader Context 中的 `pclts` 或对应编码值 |
| `s` | 根据完整 payload 生成的请求签名 |

本项目希望根据真实状态动态构造这些字段，而不是维护一份固定 `data` 模板。


### 5.5 阅读进度

当前调查显示，可以通过微信读书进度相关接口获得类似：

- chapterUid；
- chapterIdx；
- chapterOffset；
- progress；
- summary；
- readingTime；

等状态。

因此，自动阅读工具没有必要为了阅读时长上报而：

- 下载 EPUB；
- 获取完整正文；
- 解密书籍内容；
- 模拟真实翻页。

只需要取得足够的真实 Reader 和 Progress 状态。


### 5.6 Enter Report 与 Timed Report

当前 `weread.koplugin` 的实现表明，一个新的阅读 session 可以分为两个阶段。

首先发送一次进入阅读/当前位置上报。

该请求包含 Reader 和 progress 状态，但不包含主要的计时字段：

    rt
    ts
    rn
    sg

之后才开始周期性的 timed read report。

大致流程：

    Reader Context
        ↓
    enter report
        ↓
    等待约 30 秒
        ↓
    timed report
        ↓
    等待约 30 秒
        ↓
    timed report
        ↓
    ...

本项目倾向采用这种模型，而不是启动后直接发送一个固定 `rt=30` 的请求。


### 5.7 Reader Context 生命周期

当前调查显示，没有必要每 30 秒重新读取 Reader 页面。

更合理的行为是：

    获取 Reader Context
        ↓
    连续使用一段时间
        ↓
    Context 过期或服务端拒绝
        ↓
    重新获取 Reader Context

`weread.koplugin` 当前实现中存在约 15 分钟的 Context TTL。

具体 TTL 是否适用于本项目，应在正式调查中确认。


### 5.8 Cookie renewal

本项目不希望只维护单一 `wr_skey`。

计划使用 Go HTTP Cookie Jar 管理完整 session：

    http.Client
        +
    cookiejar.Jar

renewal 成功后，服务端返回的 Cookie 应进入完整 Cookie Jar。

运行期间更新后的 session 还需要持久化，以避免：

    初始 Cookie
        ↓
    服务运行并多次 renewal
        ↓
    容器重启
        ↓
    又退回最初的旧 Cookie

计划提供持久化数据目录，例如：

    /data

启动时：

    已存在持久化 session
        ↓
    优先恢复持久化 session

否则：

    使用初始 Cookie
        ↓
    建立 session

renewal 或其他 session 更新成功后，再原子持久化最新状态。


## 6. 阅读任务行为

### 6.1 核心目标

本项目的核心目标是：

> 累计微信读书阅读时间。

不要求：

- 自动推进一本书；
- 模拟真实翻页；
- 修改阅读进度；
- 随机跳章节以制造活动。


### 6.2 单次任务

一次每日阅读任务：

1. 选择一本书；
2. 获取该书真实 Reader Context 和当前阅读进度；
3. 建立一次阅读 session；
4. 持续周期性上报；
5. 达到本次随机生成的目标阅读时长；
6. 结束任务。

一次 session 中原则上只使用一本书。

不计划像 `wxread` 当前实现一样每次 report 随机切换 book/chapter。


## 7. 阅读时长随机化

`wxread` 当前主要使用固定次数，每次约 30 秒，从而形成固定阅读时长。

本项目希望支持一个阅读时间范围。

语义例如：

    最短阅读时间：40 分钟
    最长阅读时间：70 分钟

每天任务开始时，在该范围内生成当天的目标时长：

    Day 1: 53 min
    Day 2: 68 min
    Day 3: 44 min

目标时长只在一次任务开始时生成一次。

不计划为了随机化而让每一次底层 report 使用大幅变化的时间间隔。

正常 report cadence 应尽量遵循真实 Web Reader 行为。

如果实际经过时间发生异常，例如：

- 主机 suspend；
- 程序长时间阻塞；
- 网络中断很久；

不应直接把几个小时的间隔作为一次 `rt` 上报。

这种情况应结束或重新建立当前 reading session。


## 8. 每日执行时间随机化

不使用固定：

    0 1 * * *

也不需要 V1 暴露完整 cron expression。

配置一个每日运行时间窗口即可，例如：

    01:00 - 03:00

每天为当天生成一个运行时间：

    Day 1: 01:37
    Day 2: 02:16
    Day 3: 01:54

如果窗口起止时间相同，则可以自然表示固定时间。

Go 服务自己负责每日调度，不依赖容器中的 cron。


## 9. 书籍选择

### 9.1 指定书籍

支持配置多个候选书籍。

用户指定的是一组 bookId。

例如概念上：

    book A
    book B
    book C

每天任务开始时，从指定集合中随机选择一本。

整个当天 session 使用该书。

完全随机即可。

不要求 shuffle bag，也不要求保证每一本轮流出现。


### 9.2 自动选择

如果用户没有配置 bookId：

    获取当前账号书架
        ↓
    筛选能够正常建立 Reader Context 的书籍
        ↓
    随机选择一本
        ↓
    开始当天阅读 session

这与 `wxread` 当前模式不同。

`wxread` 当前所谓的随机书籍主要来自代码或配置中的静态 book/chapter 集合，并不是动态读取当前账号书架后随机选择。


### 9.3 自动选择的筛选

V1 不需要实现复杂推荐算法。

基本原则：

> 从能够正常用于 Reader 和阅读时长上报的普通书籍中随机选择。

需要在正式调查中确认不同书架项目的数据类型和可用性。

可能需要排除：

- 非普通书籍条目；
- 无法建立 Reader Context 的项目；
- 不支持正常 Web Reader 的内容。

是否排除“已读完”书籍目前没有强需求。

只要一本书仍然可以正常用于阅读 session，就没有必要仅因为 progress=100% 而排除。


## 10. Book ID 获取辅助

因为指定书籍使用 bookId，V1 可以提供一个简单的 CLI 查询能力，例如概念上的：

    wxread books

用于输出当前书架中的：

- bookId；
- title。

这同时可以用于：

- 选择 `WXREAD_BOOKS`；
- 验证 Cookie 是否有效；
- 验证书架访问是否正常。

这不是 Web UI，也不需要复杂交互。


## 11. 错误恢复

V1 应避免无限重试。

当前倾向的恢复顺序：

    read report failure
        ↓
    refresh Reader Context
        ↓
    retry
        ↓
    still failed
        ↓
    renew session
        ↓
    refresh Reader Context
        ↓
    retry
        ↓
    still failed
        ↓
    task failed

具体错误分类和重试条件应根据正式协议调查确定。

Cookie/login 已失效应该作为明确错误，而不是普通的“任务失败”。


## 12. 通知

V1 暂时支持两种通知渠道。


### 12.1 Bark

支持 Bark HTTP API。


### 12.2 企业微信机器人

支持企业微信群机器人 Webhook。

这里只考虑群机器人的 Webhook 模式。

不计划实现：

- 企业微信应用；
- CorpID；
- Secret；
- OAuth 等完整企业微信应用体系。


### 12.3 多通知渠道

通知渠道不采用互斥的：

    PUSH_METHOD=bark

这种模式。

哪个渠道配置了就启用哪个。

因此自然支持：

- 仅 Bark；
- 仅企业微信；
- Bark + 企业微信；
- 完全不配置通知。


### 12.4 通知内容

任务成功时至少应能够表达：

- 任务完成；
- 计划阅读时间；
- 实际累计阅读时间；
- report 次数；
- 本次使用的书籍。

任务失败时至少应能够表达：

- 失败阶段；
- 主要错误；
- 是否已经执行 Context refresh / session renewal 等恢复尝试。

如果确定是 Cookie 或登录状态失效，应明确通知：

> 微信读书登录状态已失效，需要重新获取初始 Cookie。

通知发送本身失败不应改变已经完成的微信读书任务结果。


## 13. 初步配置语义

下面记录的是当前讨论形成的配置语义。

具体环境变量名称仍可在正式设计时调整，不要求实现必须沿用这些名称。

    WXREAD_COOKIE=

    WXREAD_BOOKS=

    RUN_WINDOW_START=01:00
    RUN_WINDOW_END=03:00

    READ_MINUTES_MIN=40
    READ_MINUTES_MAX=70

    BARK_URL=
    WECOM_WEBHOOK_URL=

    TZ=Asia/Shanghai

运行参数不应为了“可配置”而全部暴露。

例如以下内部实现参数目前没有必要成为用户配置：

- HTTP timeout；
- retry count；
- Reader Context TTL；
- report interval；
- User-Agent；
- fallback reader token；
- log level。

除非实际使用产生调整需求，否则保留合理的内部默认值即可。


## 14. 初步代码职责划分

项目规模应保持较小。

当前讨论中的概念结构为：

    cmd/
    └── wxread/
        └── main.go

    internal/
    ├── config/
    │   └── config.go
    ├── weread/
    │   ├── client.go
    │   ├── reader.go
    │   ├── protocol.go
    │   └── report.go
    └── notify/
        ├── notify.go
        ├── bark.go
        └── wecom.go

这只是职责边界示意，不是必须遵循的目录规格。

主要职责应保持分离：

- HTTP/session/Cookie；
- Reader Context；
- 微信读书协议编码与签名；
- reading session/report 状态机；
- scheduling；
- notification；
- configuration。

`main` 不应承担协议签名等底层逻辑。


## 15. V1 非目标

V1 暂不考虑：

- Web UI；
- 多账号；
- 用户名密码登录；
- 自动浏览器登录；
- 自动抓浏览器 Cookie；
- 二维码登录；
- 数据库；
- HTTP API；
- Prometheus；
- EPUB 下载；
- 完整书籍正文获取；
- 内容解密；
- 阅读器；
- 模拟翻页；
- 主动推进阅读进度；
- 一本 session 中不断随机换书；
- 复杂推荐算法；
- 完整 cron expression；
- Telegram；
- PushPlus；
- WxPusher；
- ServerChan；
- 其他大量通知平台；
- 复杂配置文件与环境变量双配置体系。

保持 V1 范围小于 `weread.koplugin`，同时避免继承 `wxread` 中为了快速工作而形成的静态 request template 和 workaround。


## 16. 当前需要后续独立验证的技术事实

目前没有明显的产品决策阻塞项。

正式设计/实施前，仍值得重新验证以下事实。


### 16.1 Web Reader

确认当前微信读书 Web Reader：

- `window.__INITIAL_STATE__` 的实际结构；
- `reader.token` 来源；
- `psvts` / `pclts` 的当前语义；
- Reader Context 的生命周期。


### 16.2 签名

重新验证：

- bookId/chapterUid 编码算法；
- `appId` 生成方式；
- `sg`；
- `s`；
- payload 排序与 URL encoding 规则；
- 默认 reader token 是否仍有 fallback 价值。


### 16.3 `/web/book/read`

确认：

- enter report 是否仍然必要；
- timed report 当前完整 payload；
- report cadence；
- 成功响应的判定；
- `succ` / `synckey` 当前语义；
- 连续 report 是否需要更新其他 session state。


### 16.4 Cookie/session

确认：

- `/web/login/renewal` 当前请求格式；
- renewal 返回 Cookie；
- Go Cookie Jar 的保存/恢复需要持久化哪些属性；
- session 真正失效时的可识别响应。


### 16.5 Progress

确认当前 progress API：

- endpoint；
- 返回结构；
- chapterUid；
- chapterIdx；
- chapterOffset；
- progress；
- summary；

以及这些字段和 `/web/book/read` payload 的对应关系。


### 16.6 Shelf

确认当前获取书架的最佳接口以及：

- 返回的数据类型；
- bookId；
- title；
- 普通书籍与其他内容的区分；
- 哪些条目可以建立正常 Reader Context。

`weread.koplugin` 当前还涉及微信读书官方 Skill/API 相关能力，因此需要区分：

- 官方 Skill/API 能力；
- Web Cookie API；
- Web Reader 私有协议；

不要因为参考项目同时使用多套接口，就假设本项目必须采用同样的认证和接口组合。


### 16.7 长时间无人值守

验证：

- Cookie renewal 在长期运行中的实际稳定性；
- 容器重启后的 session 恢复；
- Reader Context 失效；
- 网络异常；
- 主机 suspend；
- 微信读书接口变化；

这些情况对状态机的影响。


## 17. 许可与参考方式

`weread.koplugin` 当前使用 AGPL-3.0-only。

本项目如果希望保持独立实现，应区分：

- 参考公开协议研究和可观察行为；
- 直接复制项目源代码。

当前倾向是根据协议事实自行实现 Go 版本，而不是复制其 Lua 实现。

`wxread` 的许可状态也应在正式 workflow 中重新确认。

许可问题不影响对公开 HTTP 协议行为进行独立调查，但会影响源代码复制、修改和衍生作品的处理方式。


## 18. 当前已形成的 V1 产品模型

整体流程可以概括为：

    container start
        ↓
    load persisted session
        │
        └─ unavailable → initialize from WXREAD_COOKIE
        ↓
    schedule today's random run time
        ↓
    task starts
        ↓
    renew session
        ↓
    choose book
        │
        ├─ configured book IDs → random one
        │
        └─ no configured books → random usable shelf book
        ↓
    load real progress
        ↓
    load Reader Context
        ↓
    enter report
        ↓
    periodic timed reports
        ↓
    accumulate randomly selected target duration
        ↓
    task complete
        ↓
    persist current session
        ↓
    notify configured channels
        ↓
    schedule next day

异常路径：

    report rejected
        ↓
    refresh Reader Context
        ↓
    retry
        ↓
    renew session if necessary
        ↓
    bounded final retry
        ↓
    success or explicit failure notification


## 19. 当前设计原则

当前讨论形成的主要原则：

1. 保持项目小。
2. V1 只解决自动累计阅读时间。
3. 使用真实 Reader/Progress 状态，不维护神秘的静态 request template。
4. 尽可能理解和实现协议，而不是积累 workaround。
5. 运行时间和任务时长采用简单、可解释的随机范围。
6. 一个 reading session 使用一本书。
7. 自动选书来自真实账号书架，而不是内置公共 book/chapter ID。
8. Cookie/session 是持久状态。
9. 重试必须有界。
10. 登录失效必须明确通知。
11. Docker 只负责运行和部署，不承载 cron 等额外进程。
12. 配置保持最少，不为内部参数制造无必要的用户配置。
13. 不因为参考项目功能丰富而扩大 V1 范围。
14. 协议调查结果允许后续正式 workflow 推翻或修正。


## 20. 文档定位

本文是 V1 的前期需求与调查记录。

其中：

- 产品目标和明确需求代表当前已经讨论形成的方向；
- 目录结构、配置名称等属于候选设计；
- 微信读书协议细节属于当前研究结论；
- 后续正式调查可以修正协议结论和具体设计；
- 本文不定义后续 workflow、skill 或 agent 应采用的工作方法。
