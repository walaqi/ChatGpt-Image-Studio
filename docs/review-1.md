# `multi-tenant-redesign.md` Review 1

## 结论

当前方案的大方向是对的：去掉号池、改成每用户自带 `base_url + api_key`、并把历史数据隔离到用户维度，都是必要改造。

但按现有计划直接落地，至少还有 7 个必须先补齐的问题。其中前 4 个是阻断级缺口，会直接导致多租户隔离失效、核心功能不可用，或者把危险的管理能力继续暴露给终端租户。

## Findings

### 1. [阻断] 计划没有覆盖“图片任务队列 / SSE / 任务查询”的多租户隔离

计划文档只把 `userID` 串到了“凭证解析”和“历史会话”两条线，见 `docs/multi-tenant-redesign.md:32-35` 与 `docs/multi-tenant-redesign.md:131-163`。但当前系统里，图片任务本身是另一套独立的全局内存状态。

证据在代码里很直接：`imageTaskManager` 用全局 `tasks`、`order`、`subscribers` 保存所有任务和所有订阅者，见 `backend/api/image_task_manager.go:33-44`；`listTasks()` 直接返回全部任务，见 `backend/api/image_task_manager.go:96-109`；`subscribe()` 是全局广播，见 `backend/api/image_task_manager.go:217-235`；`snapshotLocked()` 汇总的是全局队列，见 `backend/api/image_task_manager.go:1077-1124`。对应的 `GET /api/image/tasks`、`GET /api/image/tasks/{id}`、`DELETE /api/image/tasks/{id}`、`GET /api/image/tasks/stream` 也都没有任何按用户过滤，见 `backend/api/image_task_handlers.go:30-123`。

这意味着即使历史会话已经按 `userID` 隔离，只要任务层不隔离，A 用户仍然可以看到 B 用户的排队信息、处理中状态、失败原因，甚至取消 B 用户的任务。`/api/image/tasks/stream` 的初始 payload 还会把全局任务快照直接推给任意登录用户。

建议把“任务”提升为和“历史”同级的多租户对象。至少要做四件事：给 `imageTask` 增加 `UserID` 字段；`imageTaskManager` 的 `tasks/subscribers/snapshot` 按用户分区；所有任务 handler 从 `Context` 取 `userID` 再过滤；补一组跨用户任务不可见、不可取消、SSE 不串流的回归测试。

### 2. [阻断] 计划里“给 `/v1/files/image/*` 加 JWT 鉴权”会直接打断当前图片展示链路

计划在 `docs/multi-tenant-redesign.md:156-163` 与 `docs/multi-tenant-redesign.md:249` 明确要求给 `/v1/files/image/*` 加 JWT 鉴权，并校验文件归属。安全方向没问题，但它和当前前端实现并不兼容。

现在前端渲染图片是直接走普通 `<img src>`。`AppImage` 本质就是原生 `img`，见 `web/src/components/app-image.tsx:1-9`；历史和会话 UI 都直接把 `image.url` / `source.url` 放进 `src`，见 `web/src/app/image/components/history-sidebar.tsx:136-145`、`web/src/app/image/components/conversation-turns.tsx:227-235`；URL 只是被拼成绝对路径，不会附带 `Authorization` header，见 `web/src/app/image/view-utils.ts:4-18`。另外，浏览器模式下还会主动 `fetch(image.url)` 把服务端图片转成 Data URL，当前这条路径同样不带认证头，见 `web/src/store/image-conversations.ts:242-248`。

如果文件接口改成“只有 Bearer JWT 才能访问”，那历史缩略图、对话结果图、源图复用、浏览器模式下的图片物化都会直接 401。这个问题不是“前端顺手改一下 header”能解决的，因为 `<img src>` 天生就不能附带自定义鉴权头。

建议先在方案里选定一种可落地的文件访问模型，再继续实现。可选方案只有几类：用同域 `HttpOnly` 会话 cookie 承载 iframe 身份；给图片返回短期有效的签名 URL；或者前端走带鉴权的 `fetch` 再转成 Blob URL。当前计划不能只写“加 JWT 中间件”，必须把这件事设计完整。

### 3. [阻断] “在 `runPureCPAImageRequest` 里从 `request context` 取 `userID`”无法覆盖异步任务路径

计划在 `docs/multi-tenant-redesign.md:117-121` 要求 `runPureCPAImageRequest` 从 `r.Context()` 取 `userID`，然后调用 `credResolver.Resolve(ctx, userID)` 拿当前用户的 `base_url/api_key`。这只对同步请求链成立，对当前系统更常用的异步任务链并不成立。

任务调度器启动执行单元时，用的是 `context.Background()`，见 `backend/api/image_task_manager.go:398`；随后在 goroutine 里执行 `runUnit()`，见 `backend/api/image_task_manager.go:422-433`；真正执行业务时还会人为构造一个假的请求对象 `httptest.NewRequest(...)`，见 `backend/api/image_task_execution.go:45`。也就是说，异步任务并不会天然保留原始 HTTP 请求上的租户身份。

如果按计划只改同步请求链，不改任务模型，那么 `/api/image/tasks`、兼容 OpenAI 的 `/v1/images/*` / `/v1/responses` 这些最终会落到任务系统的路径，在切到“每用户凭证解析”后就会拿不到 `userID`。轻则任务全部失败，重则意外回退到默认凭证，重新变回“共享上游账号”。

建议把 `userID` 显式存进 `imageTask`，并在后台执行时重新注入到 `Context` 或 fake request 中。否则这次改造只能覆盖同步直连，不会真正覆盖项目现有主路径。

### 4. [阻断] 历史导入接口仍然暴露了“用户可控存储后端/目标地址”，不适合继续留在租户面

计划在 `docs/multi-tenant-redesign.md:151-154` 里保留了 `handleImportImageConversations`，只是改成“按当前用户导入、Clear 只清当前用户”。这还不够，因为这个接口目前不是单纯导入 JSON 数据，它允许调用方指定后端存储实现和连接目标。

当前 `handleImportImageConversations` 会从请求体读取 `storage.backend`、`imageDir`、`sqlitePath`、`redisAddr`、`redisPassword`、`redisDb`、`redisPrefix`，然后用这些值临时构造一个 `imagehistory.Store`，见 `backend/api/image_history_handlers.go:123-166`。在单用户管理台里这已经很强，在多租户 iframe 场景里继续把它暴露给终端用户就不安全了。

风险点很明确：租户可以让服务端主动连接任意 Redis 地址，或者让服务端读写任意可访问的 SQLite / 文件路径。这已经不是“导入历史”接口，而是一个带内部网络/存储控制面的管理接口。

建议把这个接口直接从租户路径删除，或者只保留在单独的 admin/internal 路由下。终端用户如果需要导入历史，接口只应该接收会话内容本身，绝不能再接收任何后端存储坐标。

### 5. [中] 计划只改了服务端隔离，没处理浏览器本地缓存的跨用户串数据问题

多租户不仅是后端隔离，还要考虑“同一浏览器里切换不同母系统用户”的情况。当前计划没有覆盖这部分。

前端认证信息目前保存在固定 key `chatgpt2api_auth_key` 下，见 `web/src/store/auth.ts:5-33`。图片历史的浏览器缓存也使用固定的 localforage / localStorage key，见 `web/src/store/image-conversations.ts:95-143`。这意味着只要同一个浏览器环境里先后打开两个不同用户的 iframe，会出现旧 JWT、旧历史、旧存储模式残留的问题。

如果未来仍保留浏览器历史模式，这会造成明显的数据串读风险；如果只保留服务端历史，至少也要在用户切换时明确清理旧缓存，否则 UI 会先渲染出上一个用户的本地快照，再被服务端数据覆盖。

建议在设计上二选一：要么嵌入版强制 `server` 历史模式，禁用浏览器模式；要么把所有本地 key 都按 `sub/userID` 做命名空间，并在 JWT `sub` 变化时清理旧租户缓存。

### 6. [中] 子路径嵌入的改造范围不完整，当前前端路由仍然假设自己部署在站点根路径

计划在 `docs/multi-tenant-redesign.md:209-217` 提到了 `vite.config.ts` 的 `base = '/image-studio/'`，以及 Go 静态托管要配合调整。但这只解决了静态资源路径，还没解决前端路由本身。

当前前端入口使用的是没有 `basename` 的 `BrowserRouter`，见 `web/src/main.tsx:7-11`。应用内路由也都写成了根路径形式，见 `web/src/App.tsx:16-24`。401 之后会直接跳 `/login`，见 `web/src/lib/request.ts:52-55`；导航项同样硬编码 `/image`、`/accounts`、`/settings`、`/requests`，见 `web/src/components/top-nav.tsx:28-33`。

这意味着即使静态资源能从 `/image-studio/` 加载，客户端导航、刷新、重定向仍会跳回站点根路径，和“iframe / 子路径嵌入”目标不一致。计划里应该把“路由 basename、重定向、菜单链接、登录跳转、父子页面通信路径”一起列入改造范围。

### 7. [中] 资源路径改成 `/<userID>/<filename>` 后，现有一批“扁平文件名”辅助逻辑也要一起改，计划里没写全

计划已经识别到图片文件需要从扁平目录改成 `data/tmp/image/<userID>/...`，这一点是对的，见 `docs/multi-tenant-redesign.md:146-163`。但当前代码里还有不少地方默认文件名一定是扁平的，只改 `saveAsset()` 和下载路由还不够。

例如文件下载 handler 会把路径中的 `/` 直接替换成 `-`，见 `backend/api/files.go:114-116`；任务执行里复用源图时也会这样处理，见 `backend/api/image_task_execution.go:214-221`；历史清理逻辑只保留 URL 的 basename，再去删本地文件，见 `backend/internal/imagehistory/store.go:380-419`。这些逻辑一旦遇到 `/<userID>/<filename>` 形式的 URL，就会把原本的目录语义打平，导致再次编辑、重试、删除清理出现误判或失效。

建议把这部分显式补进计划：不仅要改“存”和“取”，还要改所有 URL 解析、文件定位、清理引用、源图复用的辅助函数，并补一组“生成后再次编辑 / 删除会话 / 清空历史”的回归用例。

## 建议的落地顺序

不建议按当前文档直接做“大爆炸式删除”。更稳妥的顺序是：

第一步，先定死 iframe 身份模型和图片文件访问模型。这里至少要同时定下 JWT / cookie / signed URL 中的一个组合，否则后面的后端隔离改完也跑不通前端。

第二步，在不删除旧模块的前提下，把 `userID` 先贯穿四条链路：HTTP 鉴权、任务系统、历史存储、图片文件访问。等这四条链路都能通过测试，再开始删号池和同步模块。

第三步，再做物理删除和配置收缩。当前 `Server`、`main.go`、`SetupRouter()`、配置热加载、任务系统都和旧模块有深耦合，直接从计划起手删文件，返工成本会很高。

## 最低测试清单

建议把下面这些用例写成阻断测试，否则这次改造很难证明“真的多租户”：

`userA` 看不到 `userB` 的历史、任务列表、SSE 事件、图片文件。

`userA` 无法取消 `userB` 的任务，也无法复用 `userB` 的源图继续编辑。

同一浏览器先后切换两个用户时，不会短暂渲染出上一个用户的本地历史或继续使用上一个用户的 JWT。

子路径 `/image-studio/` 下，页面刷新、401 跳转、菜单导航、图片加载都正常。
