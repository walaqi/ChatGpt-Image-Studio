# 多租户改造方案 v2（每用户自带 api-key + 历史隔离 + 会话身份）

> 状态：**评审 1 已整合，待复审**
> 目标：把 ChatGpt-Image-Studio 从「单实例共享账号池」改造为「嵌入母系统的多租户图片工作台」。
> 每个终端用户使用自己的渠道 api-key 调用，历史对话、图片资源、图片任务全部按用户隔离。
>
> 变更记录：
> - v1 → v2：采纳 `docs/review-1.md` 全部 7 条；身份模型从「JWT 全程 Bearer」改为「一次性 JWT 入口验签 → 同域 HttpOnly 会话 cookie 统一身份」；新增任务系统多租户、异步路径 userID 透传、导入接口收紧、浏览器缓存命名空间、子路径路由完整化、扁平文件名辅助逻辑改造。

### 0.1 路径模型总约束（本版定稿，消除评审 2 的歧义）

为避免 `/image-studio` 子路径与后端裸路由混淆，本方案固定采用下面这组规则：

1. **浏览器对外访问路径**一律带前缀 `/image-studio`：
   - `POST /image-studio/auth/session`
   - `GET|POST /image-studio/api/...`
   - `GET|POST /image-studio/v1/...`
   - 图片文件：`/image-studio/v1/files/image/<userID>/<filename>`
2. **后端内部 `http.ServeMux` 注册路径**保持裸路径，不感知子路径：
   - `POST /auth/session`
   - `GET|POST /api/...`
   - `GET|POST /v1/...`
3. **反代负责剥前缀**：nginx `location /image-studio/ { proxy_pass http://image-studio-backend:7000/; }`
   浏览器请求 `/image-studio/v1/images/generations` 时，image-studio 后端实际收到 `/v1/images/generations`。
4. **前端统一基址**：生产环境 `webConfig.apiUrl = '/image-studio'`；开发环境保留 `http://127.0.0.1:7000`。
5. **文档中凡提“路由注册”都写裸路径，凡提“浏览器访问 / 前端调用 / 返回给前端的 URL”都写带 `/image-studio` 的对外路径。**

> 下面各章节按这个约束解释；若与旧描述冲突，以本节为准。

---

## 1. 目标与最终形态

母系统已有：用户注册、用户账号、每用户的渠道 `api_key` + `base_url`。
本应用（image-studio）以 **同源子路径 / iframe** 形态嵌入母系统，复用整套图片工作台 UI。

### 1.1 已确定的架构决策

| 维度 | 决策 |
|---|---|
| 部署形态 | **同源反代**：母系统域下 `/image-studio/*`（**不是跨域 iframe**，见 §1.3） |
| 身份传递 | 母系统签发 **一次性 JWT（含 userID）** 作为入口票据 |
| 会话身份 | image-studio 验签后，在**自己域下发 HttpOnly 会话 cookie**；之后 API + 图片文件 + SSE **统一走 cookie**（单轨，非双轨） |
| 凭证来源 | image-studio **回调母系统内部接口**用 userID 换取凭证；**两段式**（列候选 → 用户选 key → 记住 → 按 key_id 取明文），母系统只读不铸（见 §4.2） |
| 隔离对象 | 历史会话、图片资源、**图片任务/队列/SSE**（三者同级，全部按 userID 隔离） |
| 账号池等 admin 功能 | **物理删除**，只保留 cpa 链路 + 多租户 |

### 1.2 目标请求流

```
母系统页面
  └─ iframe / 跳转 ──▶ 母系统域/image-studio/?ticket=<一次性JWT>   (同源反代)
       │
       ▼  ① 入口验签（仅一次）
   前端检测 ticket → POST /image-studio/auth/session  (携带 JWT)
       │   后端：RS256 验签 + exp/iss/aud 校验 → 取 userID
       │        → 签发 image-studio 自有会话 cookie
       │        → Set-Cookie: studio_sid=<signed{userID,exp}>; HttpOnly; Secure; SameSite=Lax; Path=/image-studio
       ▼  ② 之后所有请求自动带 cookie（API / <img src> / SSE 同一身份）
   后端会话中间件：读 cookie → 验签 → userID → 注入 r.Context()
       ├─ 图片请求 ─▶ userID → 取已记住的 key_id ──回调──▶ 母系统 /internal/cred?uid=X&key_id=K
       │            ─▶ {api_key, base_url, model}（TTL 缓存）→ 构造 cpaImageClient → 请求你的渠道
       │            （首次无记住的 key：先 GET /internal/cred/keys?uid=X 列候选 → 用户选 → 记住）
       ├─ 历史请求 ─▶ userID → store 按 userID 过滤读写
       ├─ 任务请求 ─▶ userID → 任务管理器按 userID 分区（列表/SSE/取消）
       └─ 图片文件 ─▶ cookie 自动带 → 访问 /image-studio/v1/files/image/<userID>/<filename> → 校验文件归属 userID
```

### 1.3 关键前提：同源部署（cookie 模型的硬约束）

HttpOnly 会话 cookie 模型**要求 image-studio 与母系统同源**（同一站点域名，image-studio 挂在 `/image-studio/*` 子路径，由母系统反向代理）。原因：

- 同源 → cookie 是第一方 cookie，`SameSite=Lax` 即可，浏览器不会拦。
- 若做成**跨域 iframe**（image-studio 独立域名），cookie 变第三方 cookie，必须 `SameSite=None; Secure`，且会被 Safari ITP / Chrome 第三方 cookie 限制拦截，方案失效。

> **复审确认项**：~~母系统能否把 image-studio 反代到自身域名子路径？~~ ✅ **母系统已确认能同源反代**（comments-from-mother.md §A）。唯一硬前提：nginx `location /image-studio/` 必须排在母系统兜底 `location /` 之前，否则被母系统内嵌 SPA 抢路由（见 §9.3 坑 3）。无法同源时的回退方案见 §10。

---

## 2. 现状判定（改造起点）

当前系统是**彻底的单租户**设计。已核实的接缝与隔离缺口：

| 接缝 | 现状 | 关键代码 |
|---|---|---|
| **身份** | 单一共享口令 `App.AuthKey`，token 即身份，无用户维度 | `requireUIAuth` `server.go:1576`、`requireImageAuth` `server.go:1586`、`hasExactBearer` `server.go:1609`、`handleLogin` `server.go:434` |
| **凭证** | 全局唯一 `[cpa] api_key`，请求里拿不到 | `newCPAWorkflowClient` `server.go:1057`；`CPAImageBaseURL/APIKey` `config.go:459/468` |
| **历史会话** | 单一扁平命名空间，`Conversation` 无 owner，`List()` 返回全部 | backend 接口 `store.go:91`、`List` `store.go:137`、asset 内容寻址 `store.go:291` |
| **图片任务** | **全局内存状态**，`listTasks`/`subscribe`/`snapshot` 不分用户 | `imageTaskManager` `image_task_manager.go:33`、`listTasks` `:96`、`subscribe` `:217`、`snapshotLocked` `:1077`；handler `image_task_handlers.go:30-123` |
| **异步执行** | 用 `context.Background()` + 伪造 `httptest.NewRequest`，天然丢失租户身份 | `image_task_manager.go:398/422`、`image_task_execution.go:45` |
| **图片文件** | `/v1/files/image/{filename}` 无鉴权；扁平文件名，多处辅助逻辑假设无目录 | 路由 `server.go:426`、`files.go:114`、`image_task_execution.go:214`、清理 `store.go:380-419` |

好消息：cpa 链路已完整实现（`runPureCPAImageRequest` `server.go:1091` 完全绕开账号池），凭证接缝浅。难点在于把 userID 串到**四条链路**（鉴权、凭证、历史、任务），尤其是异步任务这条现有主路径。

---

## 3. 落地顺序（评审建议，不做大爆炸式删除）

> 评审明确反对"先删号池再补隔离"。改为：**先定身份与文件模型 → 不删旧模块先把 userID 贯穿四条链路并通过测试 → 最后才物理删除与配置收缩**。

| 阶段 | 内容 | 依赖 | 不可跳过的理由 |
|---|---|---|---|
| 0 | 与母系统对齐契约：JWT claim、`/internal/cred`、签名算法、反代同源、模型名 | 阻塞项 | 接口未定，后端隔离改完也跑不通 |
| 1 | 后端：入口验签 + 会话 cookie + 会话中间件 + userID context（身份） | 0 | 所有隔离的身份来源 |
| 2 | 后端：凭证 resolver + 缓存 + cpa 按请求注入 + 模型名参数化（凭证） | 1 | — |
| 3 | 后端：历史 + 图片资源按 userID 隔离 + 扁平文件名辅助逻辑改造（历史） | 1 | 评审 #7：不止改存取 |
| 4 | 后端：**任务系统** userID 分区 + 异步路径 userID 透传（任务） | 1 | 评审 #1/#3：现有主路径 |
| 5 | 前端：会话引导 + 子路径路由完整化 + 浏览器缓存命名空间 + 页面裁剪 | 1 | 评审 #5/#6 |
| 6 | 跨用户隔离回归测试全绿（§8 清单） | 1-5 | 证明"真多租户" |
| 7 | **物理删除**账号池整套 + 配置精简 + 构造链改造 | 1-6 | 评审：耦合深，最后做返工最少 |

---

## 4. 逐文件改动清单

### 4.1 身份：入口验签 + 会话 cookie + 中间件（阶段 1）

**新文件 `backend/internal/identity/jwt.go`** — 入口票据验签
- 引入 `github.com/golang-jwt/jwt/v5`（母系统同库，`v5.2.2`）。
- **RS256**：母系统私钥签，image-studio 仅持公钥验（被攻破也无法伪造）。母系统现有会话 JWT 是 HS256、且无现成应用级 RSA 密钥，需新建一对密钥（母系统侧 D2），公钥经 `[identity] jwt_public_key_path` 下发给 image-studio。
- `VerifyEntryJWT(publicKey, iss, aud) (userID string, jti string, err error)`：验签 + 校验 `exp`/`iss`/`aud`，返回 `sub`（userID）与 `jti`。
- 入口 JWT 应为**短有效期、一次性**（≤60s），仅用于换会话，不长期暴露。
- **一次性 jti 由 image-studio 侧记录**（它持公钥、负责验签）：验签通过后把 `jti` 写入黑名单（sqlite/redis，TTL=60s），重复使用同一 ticket 直接拒绝。母系统只负责在 claim 里塞 `jti`（D8 无需由母系统记黑名单）。

**新文件 `backend/internal/identity/session.go`** — 自有会话（无状态，推荐）
- 会话 cookie 值 = image-studio 自己 HMAC 签名的 `{userID, exp}`（用 image-studio 自有 secret，与母系统 JWT 公钥无关）。
- `MintSession(userID) string` / `VerifySession(cookieVal) (userID, error)`。
- **无状态**：不需要服务端 session store，天然支持 redis-less / 多实例部署。
- 备选：不透明 sessionID + redis 存 `sid→userID`（需要服务端可撤销时用）。

**新文件 `backend/internal/identity/context.go`**
```go
type ctxKey struct{}
func WithUserID(ctx, userID) context.Context
func UserIDFromContext(ctx) (string, bool)
```

**`backend/api/server.go`**
- 新增 `POST /auth/session`（入口验签 → `Set-Cookie`）：
  - `Set-Cookie: studio_sid=<signed>; HttpOnly; Secure; SameSite=Lax; Path=/image-studio; Max-Age=<会话时长>`
- 新增会话中间件 `requireSession`：读 cookie → `VerifySession` → 注入 userID context；失败 401。
- `requireUIAuth`（`:1576`）、`requireImageAuth`（`:1586`）→ 全部替换为 `requireSession`。
- `hasExactBearer`/`hasAnyBearer`/`parseKeys`（`:1596/1609`）、`cfg.App.APIKey` comma-list 多 key 逻辑：终端用户路径删除（保留与否见 §6 admin 通道）。
- `/auth/login` 单口令登录（`handleLogin` `:434`）：终端用户侧停用。

> 说明：这里的 `POST /auth/session` 指 **后端内部注册路径**。浏览器对外实际访问的是 `POST /image-studio/auth/session`，由反代剥前缀后转到该路由（见 §0.1 / §9.3 坑 1）。

**JWT 入口票据 claim 约定（已与母系统对齐定稿）**
```json
{
  "sub": "12345",          // userID（母系统 int64 字符串化；image-studio store 列为 TEXT，兼容）
  "iss": "sub2api",        // 定稿值
  "aud": "image-studio",   // 定稿值
  "exp": 1718348460,       // 短期，≤60s
  "iat": 1718348400,
  "jti": "<uuid>"          // 一次性防重放，由 image-studio 验签后记黑名单
}
```

### 4.2 凭证：两段式回调 + 用户选 key + 记住 + 按请求注入（阶段 2）

> **母系统已定稿**（comments-from-mother.md §C）：策略 = **用户自选 + 记住 + 兜底自建**。`/internal/cred` 拆成**两段式**——母系统**只读不铸**，由用户从自己能出图的 key 里挑一把，image-studio 记住选择；没有可用 key 时引导用户回母系统创建。原因：image-studio 后端持的是自己的会话 cookie，**不能**以用户身份直接调母系统的用户级接口，候选数据必须走 `/internal/*` 的 service-to-service 通道按 uid 取。

**新文件 `backend/internal/credential/client.go`**
```go
type KeyCandidate struct {
    KeyID     int64   `json:"key_id"`
    Name      string  `json:"name"`
    Quota     float64 `json:"quota"`
    QuotaUsed float64 `json:"quota_used"`
    ExpiresAt string  `json:"expires_at"`
    GroupName string  `json:"group_name"`
}
type Credential struct {
    APIKey  string // 明文，仅内存持有，不落盘、不写日志（§7.8）
    BaseURL string
    Model   string // 渠道模型名，替换写死的 gpt-image-2 / gpt-5.4-mini
}
type Resolver interface {
    // 操作 1：列候选（不含明文 key），供前端渲染 key 选择器
    ListKeys(ctx context.Context, userID string) (KeyListResult, error)
    // 操作 2：按用户选定的 keyID 取明文凭证（母系统返回前再校验归属/额度/出图条件）
    Resolve(ctx context.Context, userID string, keyID int64) (Credential, error)
}
type KeyListResult struct {
    Keys         []KeyCandidate
    CanCreate    bool   // 是否能去母系统建图片 key
    ImageGroupID *int64 // 兜底自建应绑定的预设图片 group；无可用 group 时为 nil
}
```

**两段式接口调用（对齐母系统 §C.1）**
- 操作 1 — `GET {母系统内网地址}/internal/cred/keys?uid=<userID>`：返回该用户**当前能出图**的 key 列表（母系统按"OpenAI 平台 + group 开 `allow_image_generation` + active 未过期有额度"三道门槛过滤），**不含明文**。
- 操作 2 — `GET {母系统内网地址}/internal/cred?uid=<userID>&key_id=<id>`：按选定 keyID 返回 `{ api_key, base_url, model }`。
- 两个接口都带 **service-to-service 鉴权**（`X-Internal-Secret` header，§7.2）。

**image-studio 侧"记住选择"**
- 新增极小持久化：`userID → 选定 keyID`（复用现有 sqlite/redis/file 存储，新增一张表/一个 key 前缀即可）。
- 解析流程：取请求 userID → 查"已记住的 keyID" →
  - 有记住的 → 直接走操作 2 取明文（命中即用，不打扰用户）；操作 2 报"该 key 已失效/无额度/不再满足出图条件" → 清除记住值，回退到下一步。
  - 无记住的 → 走操作 1 列候选 → 前端弹 key 选择器让用户选（§4.6）→ 选定后记住 + 走操作 2。
  - 操作 1 返回空 + `CanCreate` → 前端引导用户回母系统在图片专用 group 下建 key；`CanCreate=false`（无预设图片 group）→ 提示"管理员尚未开启图片功能"。

**TTL 缓存**
- 仅缓存**操作 2 的明文凭证**：`sync.Map` + 过期（30–60s 可配），缓存 key = `userID:keyID`。TTL 决定"用户在母系统改 key 后多久生效"。
- 操作 1 的候选列表可短缓存（如 10s）或不缓存——它只在用户没有记住值/换 key 时才走。
- 失败区分：用户无可用凭证（业务错误，前端引导）vs 母系统不可达（503，前端提示稍后重试）。

**`backend/api/server.go` + `cpa_image_client.go`** — 凭证按请求注入
- `newCPAWorkflowClient`（`server.go:1057`）：签名改为接收 `Credential`，来源从 `s.cfg.CPAImageBaseURL()/APIKey()` 改为传入。
- `runPureCPAImageRequest`（`server.go:1091`）：从 context 取 userID → 走上面的解析流程拿 `Credential` → 传 `cred`。`CPAImageConfigured()` 全局检查（`:1102`）改为"该用户凭证解析成功"；解析失败时返回明确的业务错误（区分"未选 key/无可用 key"与"母系统不可达"）。
- **写死模型名修复**：`cpaFixedImageModel="gpt-image-2"`（`server.go:74`）、`cpaResponsesMainModel="gpt-5.4-mini"`（`cpa_image_client.go:364`），被 `cpa_image_client.go:120/156/214/250/438/462` 多处引用 → 改为从 `cred.Model`/配置读取。
- `cpaImageClient` 增加 `model` 字段；`cpaClientFactory`（`server.go:45/101`）扩展接收 model 或直接接收 `Credential`。
- ⚠️ **异步路径同样要走这条注入**（见 §4.4），不能只改同步链。异步任务在创建时就该把"选定 keyID"连同 userID 一起存进 `imageTask`，执行时用 `(userID, keyID)` 取凭证——否则任务执行时用户可能已换 key 或选择器状态丢失。

**新增 image-studio 内部 API（供前端 key 选择器用，带会话 cookie 鉴权）**
- `GET /api/image/credential/keys`：取当前用户候选（后端转调操作 1）。
- `GET /api/image/credential/current`：返回当前记住的 keyID（含其是否仍有效）。
- `PUT /api/image/credential/current`：保存用户选定的 keyID。
- 这三个走 image-studio 自己的会话 cookie 鉴权，userID 从 context 取，**前端不接触 `X-Internal-Secret` 也不接触明文 key**。

> 说明：这里列的是 **后端内部注册路径**。浏览器对外实际访问的是：
> - `GET /image-studio/api/image/credential/keys`
> - `GET /image-studio/api/image/credential/current`
> - `PUT /image-studio/api/image/credential/current`

### 4.3 历史与图片资源按 userID 隔离（阶段 3）

**`backend/internal/imagehistory/store.go`**
- `backend` 接口（`:91`）所有方法加 userID：
  ```go
  List(ctx, userID) ([]Conversation, error)
  Get(ctx, userID, id) (*Conversation, error)
  Save(ctx, userID, conversation) error
  Delete(ctx, userID, id) error
  Clear(ctx, userID) error
  ```
- `Store` 包装方法（`:137-186`）同步透传 userID。
- **sqlite**：表加 `user_id TEXT NOT NULL DEFAULT ''` + 索引 `(user_id, updated_at)`；`List/Get/Delete/Clear` 加 `WHERE user_id=?`。
- **redis**：key 从 `<prefix>:image_history:conversations` 改为 `<prefix>:image_history:<userID>:conversations`（`:114`）。
- **file**：目录 `data/image_history/` → `data/image_history/<userID>/`（`defaultHistoryDir` `:25`）。
- **图片资源**（`saveAsset` `:291`）：当前 `sha256[:16]` 扁平内容寻址 → 哈希去重会**跨租户共享文件**（泄露风险）。改为 `<imageDir>/<userID>/<kind>-<hash><ext>`，URL `/v1/files/image/<userID>/<filename>`。

> 说明：这里的 `/v1/files/image/<userID>/<filename>` 指 **后端内部相对路径**。浏览器对外看到的是 `/image-studio/v1/files/image/<userID>/<filename>`；前端统一以 `webConfig.apiUrl='/image-studio'` 去拼接（见 §0.1 / §4.6）。

**`backend/api/image_history_handlers.go`**
- 所有 handler（`:12/32/56/85/104`）从 context 取 userID 传入 store。
- `handleListImageConversations`（`:24`）只返回当前用户会话。

**评审 #7：扁平文件名辅助逻辑一并改造**（不止改存取）
- `files.go:114-116`：把路径 `/` 替换成 `-` 的逻辑会打平 `<userID>/<filename>` 目录语义 → 改为正确解析 userID 子目录。
- `image_task_execution.go:214-221`：源图复用同样的 `/`→`-` 处理 → 同步改。
- `store.go:380-419`：历史清理只取 URL basename 再删本地文件 → 需带上 userID 子目录定位，否则删除/清空误判。
- `files.go:52` `gatewayImageURL` + `image.go:13/27`：OpenAI 兼容 `/v1/images/*` 响应里返回给调用方的图片 URL，必须生成**浏览器/客户端可见的外部路径** `/image-studio/v1/files/image/<userID>/<filename>`，不能继续返回根路径 `/v1/files/...`。**定稿做法（review-3 建议）**：后端新增 `[server] public_base_path = "/image-studio"` 配置，用它拼对外 URL（比读 `X-Forwarded-Prefix` 更稳定、单测好写——无需伪造反代头）。而持久化到历史里的相对路径仍保留内部形式 `/v1/files/image/<userID>/<filename>`，交给前端按 `webConfig.apiUrl` 拼接。`public_base_path` 为空时回退为不加前缀（本地直连开发场景）。
- 补回归用例："生成后再次编辑 / 删除会话 / 清空历史"在带 userID 目录下正确定位文件。

### 4.4 图片任务系统多租户（阶段 4）— 评审 #1 + #3

**`backend/api/image_task_manager.go`** — 任务提升为与历史同级的租户对象
- `imageTask` 增加 `UserID string` 字段。
- `tasks`/`order`/`subscribers`（`:33-44`）按用户分区：`subscribers` 按 userID 分组，避免全局广播；`listTasks`（`:96`）、`snapshotLocked`（`:1077`）按 userID 过滤。
- `subscribe`（`:217`）只投递该 userID 的事件。

**`backend/api/image_task_handlers.go`**（`:30-123`）
- 后端内部注册：`GET /api/image/tasks`、`GET /api/image/tasks/{id}`、`DELETE /api/image/tasks/{id}`、`GET /api/image/tasks/stream`。
- 浏览器对外访问：`GET /image-studio/api/image/tasks`、`GET /image-studio/api/image/tasks/{id}`、`DELETE /image-studio/api/image/tasks/{id}`、`GET /image-studio/api/image/tasks/stream`。
- 所有这些路径都从 context 取 userID，做过滤 + 归属校验（A 不能查/取消/订阅 B 的任务）。

**异步执行 userID 透传**（评审 #3，关键）
- 任务创建时把 context 里的 userID 存进 `imageTask`。
- 调度器启动单元用 `context.Background()`（`image_task_manager.go:398/422`）→ 改为 `identity.WithUserID(context.Background(), task.UserID)`。
- `image_task_execution.go:45` 伪造的 `httptest.NewRequest` → 注入带 userID 的 context（`req.WithContext(...)`），保证落到 `runPureCPAImageRequest` 时能取到 userID → 解析到正确用户凭证。
- ⚠️ 不改这条，切到按用户凭证后异步任务会拿不到 userID：轻则全失败，重则回退默认凭证（变回共享上游）。

### 4.5 导入接口收紧（阶段 3）— 评审 #4

**`backend/api/image_history_handlers.go:123-166`**
- 当前 `handleImportImageConversations` 从请求体读 `storage.backend/imageDir/sqlitePath/redisAddr/redisPassword/...` 临时构造 `Store` → **终端用户可让服务端连任意 Redis / 读写任意路径**，是内部网络/存储控制面，绝不能留在租户面。
- 处理：**从租户路径删除**；若仍需导入，新接口只接收会话内容本身（`items`），存储坐标一律用服务端配置，按当前 userID 落盘，`Clear` 只清当前用户。

### 4.6 前端：会话引导 + 子路径 + 缓存命名空间 + 裁剪（阶段 5）

**会话引导**（替代登录口令）
- 入口：检测 URL `?ticket=<JWT>`（或父窗口 postMessage）→ `POST /image-studio/auth/session` 换 cookie → 清除 URL 上的 ticket（避免泄露/回退重放）。
- 之后**前端不再持有/附带任何 token**：cookie 自动随同源请求发送，`<img src>` / `fetch(image.url)` / SSE 全部天然带 cookie，**无需改取图组件**。
- `web/src/store/auth.ts`、`web/src/lib/request.ts:33-44`（axios 注入 Authorization）、`api.ts:762/913`（SSE/download 手动 header）：移除 Bearer 注入逻辑，改为依赖 cookie。
- `web/src/app/login/page.tsx`：终端用户侧删除/停用。
- cookie 过期/401：向母系统父窗口请求重新引导（重新取 ticket → 换 cookie）。

**前端 API / 文件基址统一**
- `web/src/constants/common-env.ts`：生产环境 `apiUrl = '/image-studio'`；开发环境继续用 `http://127.0.0.1:7000`。
- 所有 `httpRequest("/api/...")`、`httpRequest("/v1/...")`、SSE、download、图片 URL 拼接都依赖同一个 `apiUrl`，从而在生产环境自动对外访问：
  - `/image-studio/api/...`
  - `/image-studio/v1/...`
- 这样前端无需显式写两套路由；浏览器看到的是带前缀路径，后端收到的是剥前缀后的裸路径（§0.1）。

**凭证选择器 UI**（配合 §4.2 两段式凭证）
- 新增组件：从后端 `GET /image-studio/api/image/credential/keys`（浏览器对外路径；后端内部注册 `GET /api/image/credential/keys`，再转 `/internal/cred/keys`）拿候选 key 列表渲染选择器，展示 `name / group_name / 剩余额度 / 过期时间`。
- 用户选定后 `PUT /image-studio/api/image/credential/current { key_id }`（浏览器对外路径；后端内部注册 `PUT /api/image/credential/current`）→ 后端记住 `userID → key_id`（见 §4.2「记住」）。之后出图不再追问。
- 候选为空：
  - `can_create=true` → 提示"去母系统创建图片 key"，给母系统建 key 入口链接（父窗口跳转或新开 tab）。
  - `can_create=false`（`image_group_id=null`，admin 未预置图片 group）→ 提示"管理员尚未开启图片功能"，禁用出图按钮。
- 出图前若选定 key 已失效（过期/无额度），后端 `Resolve` 返回明确错误 → 前端弹回选择器重选。
- 入口可放在工作台 header（`workspace-header.tsx`）或设置抽屉；保持轻量，默认折叠，记住选择后只显示当前 key 名。

**子路径路由完整化**（评审 #6，不止 vite base）
- `web/vite.config.ts`：`base = '/image-studio/'`。
- `web/src/main.tsx:7-11`：`BrowserRouter` 加 `basename="/image-studio"`。
- `web/src/App.tsx:16-24`：路由保持相对，由 basename 统一前缀。
- `web/src/lib/request.ts:52-55`：401 跳转不再去 `/login`，改为触发母系统重新引导。
- `web/src/components/top-nav.tsx:28-33`：硬编码 `/image`、`/accounts`、`/settings`、`/requests` → 走 basename；admin 项删除。
- Go 静态托管：`server.go:428` `handleWebApp`、`resolveStaticAsset` `:1632`、`pages.go` StripPrefix 配合子路径。
- 明确生产环境对外规则：
  - 前端页面：`/image-studio/...`
  - API：`/image-studio/api/...`
  - OpenAI 兼容接口：`/image-studio/v1/...`
  - 图片文件：`/image-studio/v1/files/image/...`
- 兼容 `/v1/images/*` 的返回体里，若 `response_format=url`，服务端返回给调用方的 `url` 字段也必须是上面的**对外路径**，而不是内部裸 `/v1/...`。

**浏览器缓存命名空间**（评审 #5，同浏览器切用户串数据）— **定稿（review-3 建议）**
- 嵌入版**强制 `server` 历史模式，禁用 browser 历史模式**。理由：同浏览器先后打开两个用户的 iframe 时，固定 localStorage/localforage key 会串读上一用户的本地历史；强制 server 模式让历史只走后端按 userID 隔离的 store，浏览器不持久化会话内容，从根上消除串读。
- 配套：前端去掉 browser/server 模式切换入口；`image-conversations.ts` 的本地缓存仅作内存态，不落 localforage/localStorage（或登出/userID 变化时清空）。
- 残留的认证态 key（`chatgpt2api_auth_key` `auth.ts:5`）随会话 cookie 模型一并移除（§4.6 会话引导）。

**页面裁剪**（`top-nav.tsx`）
- 隐藏/删除：账号管理、配置管理、请求记录、设置中的同步/代理/账号池。
- 保留：图片工作台（`web/src/app/image/*`）及历史侧栏。

### 4.7 物理删除账号池整套（阶段 7，最后做）

| 删除目标 | 路径 |
|---|---|
| 账号池存储与图片租约 | `backend/internal/accounts/*` |
| JWT token 池（遗留） | `backend/internal/token/*` |
| CPA/同步客户端 | `backend/internal/cliproxy/*`、`sub2api/*`、`newapi/*` |
| 同步状态 | `backend/api/source_sync*.go`、`internal/configstore`（若仅 sync 用） |
| chatgpt.com 直连客户端 | `backend/handler/client.go`、`responses_client.go`（及 `pow.go`、`transport.go` 若仅其用） |
| Studio 模式分支 | `withImageResultsFilteredWithMetadata` 中 `mode != cpa` 分支（`server.go:988-1054`）、`runImageRequest*`（`:1213/1217`）、`resolveImageUpstreamModel`、`isPaidImageAccountType` |
| 账号/同步/配置 admin 路由 | `server.go:383-407` |
| 相关 handler | `account.go`、`admin.go`、`image_account_policy*.go`、`proxycheck.go`、`integration_probe.go` |
| 前端 admin 页面 | `web/src/app/accounts/*`、`settings/*`、`requests/*`、相关 store |

**构造链改造**
- `SetupRouter(cfg, store, syncClient)`（`router.go:11`）→ 去掉 `*accounts.Store`、`*cliproxy.Client`，注入 `credential.Resolver` + 会话/JWT 配置。
- `main.go`：去掉账号池/同步初始化，新增凭证 resolver、JWT 公钥加载、会话 secret。
- 每步跑 `go build ./...` 迭代清理编译错误。

### 4.8 配置精简（阶段 7）

`config.go` + `config.defaults.toml`：
- **删除**：`[accounts]`、`[sync]`、`[proxy]`、`[newapi]`、`[sub2api]`、`[chatgpt]` 的 route/model 字段、`[storage]` 的 auth_dir/state_file/sync_state_dir。
- **保留**：`[server]`、`[storage]`（backend/sqlite/redis + image_dir）、`[chatgpt]` 仅 timeout 类。
- **新增**：
  ```toml
  [identity]
  jwt_public_key_path = "data/jwt_public.pem"   # 母系统 RS256 公钥
  jwt_issuer = "sub2api"        # 定稿值，与母系统 ticket iss 对齐
  jwt_audience = "image-studio" # 定稿值
  session_secret = "image-studio-own-hmac-secret"  # 自有会话签名（与母系统公钥无关）
  session_ttl_seconds = 3600

  [server]
  public_base_path = "/image-studio"  # 对外子路径；用于兼容 /v1 响应里的图片 URL 生成（定稿，不再用 X-Forwarded-Prefix）

  [credential]
  # 两段式内部端点（同一母系统 base，路径不同）：
  #   列候选 GET <endpoint_base>/internal/cred/keys?uid=<userID>
  #   取明文 GET <endpoint_base>/internal/cred?uid=<userID>&key_id=<id>
  endpoint_base = "http://main-system-internal:8080"
  internal_secret = "service-to-service-secret"   # X-Internal-Secret
  cache_ttl_seconds = 60

  # 网关 base_url 由 image-studio 配置决定（区分生产/开发），不依赖母系统返回。
  # 母系统 /internal/cred 返回的 base_url 可作兜底，但优先用本配置。
  #   开发：http://localhost:3000/v1
  #   生产：母系统内网网关地址，如 http://main-system-internal:8080/v1
  gateway_base_url = "http://localhost:3000/v1"

  [chatgpt]
  image_mode = "cpa"   # 唯一模式
  ```

---

## 5. 数据库 / 存量迁移

**sqlite**：
```sql
ALTER TABLE image_conversations ADD COLUMN user_id TEXT NOT NULL DEFAULT '';
CREATE INDEX idx_conv_user_updated ON image_conversations(user_id, updated_at);
```
- 启动时版本化 migration（检测列是否存在）。存量 `user_id` 空值视为遗留，赋给某 owner 或清空。

**redis**：存量 `<prefix>:image_history:conversations` 脚本迁移到 `<userID>` 命名空间，或弃用。
**file**：`data/image_history/*.json` 归入 `<userID>/` 子目录，或弃用。
**图片资源**：`data/tmp/image/*` 平铺文件迁入 `<userID>/`，或弃用。

---

## 6. 可选：运维 admin 通道

若需运维侧调试，保留独立、与租户路径隔离的 admin 接口（单口令 `App.AuthKey` + 内网/IP 白名单），放 `/admin/*` 前缀，**不暴露给 iframe**。

---

## 7. 安全检查清单（强制）

1. **同源部署确认**：cookie 模型依赖同源反代（§1.3）。跨域 iframe 则改用签名 URL（§10）。
2. **`/internal/cred` 必须 service-to-service 鉴权**：否则拿 userID 即可查他人 api-key。
3. **入口 JWT 用 RS256 + 短期 + 一次性**：母系统私钥签，image-studio 仅持公钥；`exp`≤60s；用过即弃。
4. **会话 cookie**：`HttpOnly` + `Secure` + `SameSite=Lax` + `Path=/image-studio` + 合理 `Max-Age`。
5. **CSRF**：cookie 鉴权下，所有 `POST/PUT/DELETE` 需 CSRF 防护——同源 + `SameSite=Lax` 已挡大部分；额外要求自定义头（如 `X-Requested-With`）+ 严格 CORS（不允许跨站带凭证）。
6. **JWT `aud`/`iss` 校验**：防母系统其他用途 token 被直接拿来换会话。
7. **图片文件归属校验**：后端内部路由 `/v1/files/image/<userID>/<file>`（浏览器对外为 `/image-studio/v1/files/image/<userID>/<file>`）必须校验路径 userID == 会话 userID，防越权下载。
8. **凭证不落盘明文**：api-key 仅内存缓存，不写日志、不进 `request_log.go`（核对不记录凭证）。
9. **postMessage 校验 origin**（若用 postMessage 传 ticket）：严格校验 `event.origin` 为母系统域。
10. **任务/SSE 隔离校验**：订阅与快照初始 payload 不得含他人任务（评审 #1）。

---

## 8. 最低回归测试清单（阻断级，证明"真多租户"）

- `userA` 看不到 `userB` 的：历史会话、任务列表、SSE 事件、图片文件。
- `userA` 无法取消 `userB` 的任务，无法复用 `userB` 的源图继续编辑。
- **异步任务**用正确的 per-user 凭证执行（不回退默认/共享凭证）。
- 同浏览器先后切换两用户：不残留旧 JWT/cookie，不短暂渲染上一用户本地历史。
- 子路径 `/image-studio/` 下：页面刷新、401 重引导、菜单导航、图片加载均正常。
- 生成后再次编辑 / 删除会话 / 清空历史：带 userID 目录的文件定位与清理正确。
- 导入接口不接受任何存储后端坐标（backend/redisAddr/sqlitePath 等被拒绝或忽略）。

---

## 9. 部署：nginx 同源反代落地

### 9.1 同源的判定基础

浏览器"同源" = **scheme + host + port 三者完全相同**，与路径无关：

```
https://app.example.com/dashboard      ┐ 同源
https://app.example.com/image-studio/  ┘（路径不同没关系）

https://studio.example.com/...           跨域（host 不同）
http://app.example.com/...               跨域（scheme 不同）
https://app.example.com:8443/...         跨域（port 不同）
```

反向代理做的就是：让母系统与 image-studio 两个不同后端，对浏览器表现为**同一个 origin 下的不同路径**。用户浏览器只跟 `https://app.example.com` 这一个 origin 打交道，nginx 在背后按路径分发。这样 image-studio 下发的 cookie 作用域是 `app.example.com`，母系统页面里的 iframe 加载 `app.example.com/image-studio/` 时它就是第一方 cookie，畅通无阻。

同源反代不依赖任何特殊基础设施——nginx / Caddy / Traefik / 母系统自带网关任选其一，一个 `location` 块即可。基础只有一条：**对外暴露同一个 `scheme://host:port`**。

### 9.2 nginx 配置示例

> 母系统已确认（comments-from-mother.md §A.1）：其前置反代就是 nginx，照搬此套即可，母系统侧**零代码改动**。**注意 location 顺序**：`/image-studio/` 必须排在母系统兜底 `location /` 之前（见坑 3）。

```nginx
server {
    listen 443 ssl;
    server_name app.example.com;

    # image-studio 子路径——必须放在母系统兜底 location 之前（坑 3）
    location /image-studio/ {
        # 结尾的 / 让 nginx 剥掉 /image-studio 前缀再转发（见坑 1）
        proxy_pass http://image-studio-backend:7000/;

        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # 坑 2：任务流是 SSE，必须关掉缓冲
        proxy_buffering    off;
        proxy_cache        off;
        proxy_read_timeout 3600s;
        proxy_http_version 1.1;
        proxy_set_header   Connection "";
    }

    # 母系统（兜底）
    location / {
        proxy_pass http://main-system-backend:8080;
    }

    # /internal/* 绝不能经公网反代暴露——母系统的内部凭证端点（§7.2 / comments §C.2）
    # 不为它配 location（默认落兜底，但兜底应是母系统内网地址，外部本就打不到）；
    # 若母系统与公网同端口，必须显式拒绝：
    location /internal/ { deny all; return 404; }
}
```

### 9.3 三个项目特有的坑

**坑 1 — 前缀剥离要和后端 base path 对齐。**
`proxy_pass` 结尾带 `/`（`...:7000/`）时 nginx 剥掉 `/image-studio` 前缀再转发：浏览器请求 `/image-studio/v1/images/generations` → 后端收到 `/v1/images/generations`，后端路由保持原样、无需感知子路径。但前端静态资源与 SPA 路由必须感知前缀，否则 `<script src>`、客户端跳转会指向错误路径——即 §4.6 的 vite `base='/image-studio/'` + `BrowserRouter basename`。两端必须一致：**"nginx 剥前缀 + 前端加 base"**（推荐，后端零改动）或"nginx 不剥 + 后端全程感知前缀"。

**坑 2 — SSE 必须关 nginx 缓冲。**
`GET /image-studio/api/image/tasks/stream`（浏览器对外路径；后端内部路由为 `GET /api/image/tasks/stream`）是 SSE 长连接。nginx 默认 `proxy_buffering on` 会缓存流，导致前端收不到实时进度。对该 location 关 `proxy_buffering`、放大 `proxy_read_timeout`（如上）。

**坑 3 — 母系统内嵌 SPA 会吞掉 `/image-studio/*`（comments §A.2，硬前提）。**
母系统前端是 Go embed 内嵌 SPA，全局中间件在路由匹配前挂载，bypass 白名单（`/api/ /v1/ /backend-api/ ...`）**不含 `/image-studio`**。后果：任何 `/image-studio/...` 请求**只要打到母系统 Go 后端**，都会命中 SPA fallback → 返回母系统自己的 `index.html`（HTTP 200，不是 404），image-studio 永远加载不出来。
- **硬前提**：nginx 的 `location /image-studio/` **必须排在母系统兜底 `location /` 之前**（nginx 按最长前缀匹配，前缀 location 优先于兜底，顺序无关，但显式排前更不易误配）。只要反代层拦住，请求根本不进母系统 Go，母系统**零代码改动**即可共存——无需改它的 bypass 白名单。
- 验证：部署后直接 `curl https://app.example.com/image-studio/health`，应返回 image-studio 的健康响应，而非母系统 HTML。

### 9.4 会话 cookie 必须配套的属性

同源反代成立后，image-studio 下发会话 cookie（§4.1）须设：

```
Set-Cookie: studio_sid=...; HttpOnly; Secure; SameSite=Lax; Path=/image-studio
```

- `HttpOnly`：JS 读不到，防 XSS 窃取。
- `Secure`：整链路 HTTPS（同源反代对外通常即 443）。
- `SameSite=Lax`：同源场景足够；**仅在无法同源、被迫跨域 iframe 时才需 `SameSite=None; Secure`**，而那会受 Safari ITP / Chrome 第三方 cookie 新政限制——见 §1.3 与 §10 备选。
- `Path=/image-studio`：作用域限定子路径，不泄漏给母系统其他路径。

---

## 10. 备选：跨域 iframe 场景的文件访问（仅当无法同源时启用）

若母系统无法同源反代，cookie 模型失效，改为：
- API 仍走 Bearer JWT（前端持 JWT）。
- 图片文件走**短期签名 URL**：浏览器对外使用 `/image-studio/v1/files/image/<uid>/<file>?sig=<hmac>&exp=<ts>`（或独立图片域下的等价路径）；后端内部校验仍基于 `/v1/files/image/<uid>/<file>` 路由，`<img src>` 直接用，后端校验 sig 对应 uid 且未过期。
- 代价：历史列表/会话缓存/二次编辑源图复用在 TTL 过期后需重签（前端拿到的 URL 要能按需刷新）。
- 这是双轨鉴权，复杂度更高，仅作为同源不可行时的回退。

---

## 11. 接口契约（已与母系统对齐定稿）

> 母系统侧已逐条对照源码确认（comments-from-mother.md，2026-06-15），契约整体可实现、无架构级阻塞。下表为定稿结论。

| # | 契约项 | 定稿 |
|---|---|---|
| 1 | **同源反代** image-studio 到母系统域 `/image-studio/*` | ✅ 可行。母系统前置即 nginx，零代码改动，只加 `location` 块。**硬约束**：`location /image-studio/` 必须排在母系统兜底 `location /` 之前（母系统内嵌 SPA catch-all 会吞掉未匹配路径，见 §9.3 坑 3）。 |
| 2 | **入口 JWT** | ✅ RS256；claim `sub`(userID int64 字符串化) / `iss="sub2api"` / `aud="image-studio"` / `exp≤60s` / `jti`。母系统需新建 RSA 密钥对（D2）+ 签发端点（D3）。公钥经配置 `[identity] jwt_public_key_path` 下发。 |
| 3 | **一次性 jti** | ✅ 母系统在 claim 塞 `jti`；**image-studio 侧记黑名单**（持公钥、负责验签，TTL=60s）。母系统不记 Redis 黑名单。 |
| 4 | **ticket 注入** | ✅ URL query `?ticket=<JWT>`；image-studio 换 cookie 后立即清除 URL ticket。 |
| 5 | **`/internal/cred` 两段式** | ✅ `GET /internal/cred/keys?uid=`（列候选，不含明文）+ `GET /internal/cred?uid=&key_id=`（取明文 `{api_key, base_url, model}`）。均 `X-Internal-Secret` 鉴权。母系统**只读不铸**。见 §4.2。 |
| 6 | **凭证选取策略** | ✅ **用户自选 + 记住 + 兜底自建**。image-studio 渲染候选选择器，记住 `userID→key_id`，无可用 key 时引导回母系统建。 |
| 7 | **base_url** | ✅ 写在 image-studio `[credential]` 配置（区分环境）：开发 `http://localhost:3000/v1`，生产填母系统内网网关地址。 |
| 8 | **渠道 model** | ✅ 从 `/internal/cred` 返回的 `model` 字段读（母系统侧可配，默认 `gpt-image-2`），替换写死常量。 |
| 9 | **缓存 TTL** | ✅ image-studio 侧 30–60s，母系统无状态、无需改造。 |

### 11.1 剩余待办（非阻塞，落地前确认）

- **母系统 D6（部署前提，必做）**：admin 须预置**至少一个**"OpenAI 平台 + `allow_image_generation=true` + 账号映射好 `gpt-image-*`"的 group。否则"兜底自建"形同虚设——用户建了 key 也绑不到合规 group，照样不能出图。操作 1 的 `image_group_id` 即指此 group；未配置则该字段为 null、`can_create=false`，image-studio 提示"管理员尚未开启图片功能"。
- **base_url 内网可达性**：确认 image-studio 后端能否访问母系统内网网关（同 compose 网络则填内网地址，省一次公网 TLS 往返）。
- **`/internal/*` 公网隔离**：母系统侧确认该路由不在公开 `location` 内（§9.2 已给 `deny all` 兜底）。
