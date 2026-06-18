# 母系统（sub2api）侧契约评估 — multi-tenant-redesign.md

> 评估对象：`docs/multi-tenant-redesign.md` v2，重点是 §11「待母系统团队确认的接口契约」、§4.1/§4.2 的身份与凭证契约、§9 同源反代。
> 评估方法：对照 sub2api（母系统，fork: walaqi/sub2api）实际源码逐条核实，所有结论附 `文件:行号`。
> 评估人：母系统侧（Claude / sub2api 维护）。日期：2026-06-15。
>
> **状态更新（2026-06-15）：母系统侧契约已全部实现并通过真后端验证。** 分支 `feat/image-studio-integration`，5 个 commit（config / 票据 / 两段式 cred / 动态发现重构 / 前端入口）。下文 §D 是「已落地清单」（含真实端点、字段、配置项）；§E 已落地决策。image-studio 侧对接看 §C.1（接口形状）+ §D（端点/配置）+ §E（已定值）即可。

---

## 0. 总体结论（先给判定）

| 契约项 | 状态 | 判定 |
|---|---|---|
| §11.1 同源反代到 `/image-studio/*` | ✅ 母系统侧就绪（运维需配 nginx，§A.2） | 见 §A |
| §11.2 入口 JWT（RS256） | ✅ 已实现：`GET /api/v1/image-studio/ticket` | 见 §B |
| §11.3 ticket 注入方式 | ✅ URL query `?ticket=`（前端整页跳转已实现） | 见 §B.4 |
| §11.4 `/internal/cred` 接口 | ✅ 已实现：两段式（列候选/取明文），`X-Internal-Secret` 鉴权 | 见 §C |
| §11.5 凭证缓存 TTL | ✅ 无需母系统改造（image-studio 侧缓存即可） | 见 §C.5 |
| §11.6 渠道模型名 | ✅ 已实现：返回 `image_studio.image_model`（默认 `gpt-image-2`，可配） | 见 §C.6 |

**一句话结论**：契约**已全部落地并验证**。母系统新增了 **3 样东西**：(1) 一对应用级 RSA 密钥（config 文件路径加载）；(2)「签发入口 ticket」端点 `GET /api/v1/image-studio/ticket`（已登录用户调，RS256 签 `{sub,iss,aud,exp,iat,jti}`）；(3) 两段式 `/internal/cred` 内部端点（`X-Internal-Secret` 鉴权，列候选 / 取明文）。**1 个运维动作**：nginx 必须显式拦 `/image-studio/*`（排在兜底前）且**拒绝外网访问 `/internal/`**（§A.2 / §C.2）。**1 个 admin 前提**：预置至少一个可出图的图片专用 group，否则「兜底自建」无目标（§C.4，运行时动态发现，非配置写死）。凭证策略：**用户自选 + 记住 + 兜底自建**。

---

## A. 同源反代支持评估（§9 / §11.1）

### A.1 能同源——文档 §9 的 nginx 示例方向正确

文档 §9 用 nginx 举例，与母系统实际部署一致：**母系统前置反代就是 nginx**。落地只需在 nginx 配置里，为 image-studio 子路径加一个 `location` 块，并把它放在母系统兜底 `location /` 之前：

```nginx
# image-studio 子路径——必须放在母系统兜底 location 之前
location /image-studio/ {
    # 结尾的 / 让 nginx 剥掉 /image-studio 前缀再转发（见 A.3）
    proxy_pass http://image-studio-backend:7000/;

    proxy_set_header Host              $host;
    proxy_set_header X-Real-IP         $remote_addr;
    proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
    proxy_set_header X-Forwarded-Proto $scheme;

    # SSE：关缓冲、放大超时
    proxy_buffering    off;
    proxy_cache        off;
    proxy_read_timeout 3600s;
    proxy_http_version 1.1;
    proxy_set_header   Connection "";
}

# 母系统（兜底）——转给 Go 后端
location / {
    proxy_pass http://localhost:8080;
}
```

文档 §9.2 给的就是这套，照搬即可。母系统这边**无需任何代码改动**，只加 nginx 的 `location` 块。

### A.2 ⚠️ 必须处理的坑：母系统的内嵌 SPA 会吞掉 `/image-studio/*`

母系统前端是 **Go embed 内嵌**（`backend/internal/web/embed_on.go`），通过全局中间件 `FrontendServer.Middleware()`（`router.go:70-83`）在路由匹配前挂载。它的 bypass 白名单 `shouldBypassEmbeddedFrontend`（`embed_on.go`）只放行：

```
/api/  /v1/  /v1beta/  /backend-api/  /antigravity/  /setup/  /health  /responses  /responses/  /images/
```

**不含 `/image-studio`**。后果：任何 `/image-studio/...` 请求**只要打到 Go 后端**，都会命中 SPA fallback → 返回 sub2api 自己的 `index.html`（HTTP 200 + `c.Abort()`），而不是 404。

> **结论**：同源反代成立的硬前提是——**必须在 nginx 显式拦截 `/image-studio/*` 并转给 image-studio 后端，且该 `location` 块要排在母系统兜底 `location /` 之前**。否则请求落到 Go，被内嵌 SPA 抢走。
>
> 反过来说，只要反代层拦住了，请求根本不进 Go，母系统这边**零代码改动**就能共存。无需动 `shouldBypassEmbeddedFrontend`（动它也只是把「返回错误 index.html」变成「404」，治标不治本）。

### A.3 前缀剥离：建议「nginx 剥前缀 + 前端加 base」

与文档 §9.3 坑 1 一致。image-studio 后端路由（`/v1/...`、`/auth/session`、`/api/image/...`）保持不感知子路径，由前端 `vite base='/image-studio/'` + `BrowserRouter basename` 承担前缀。母系统对此无要求，纯属 image-studio 侧 + 反代配置。

### A.4 base_url 的同源含义（重要决策点，见 §C.3）

注意区分两条链路：

1. **浏览器 → `/image-studio/*`**：经反代到 image-studio 后端。这是同源 cookie 模型的作用域。
2. **image-studio 后端 → 母系统网关 `/v1/images/generations`**：这是 image-studio 拿着用户 key 作为**网关客户端**发起的 server-to-server 调用。

第 2 条链路就是母系统 gateway 的标准用法（`routes/gateway.go` 的 `/v1` group），image-studio 等于一个普通 API 客户端。**这条调用建议走母系统内网地址**（如 `http://localhost:8080/v1` 或容器内网），不必绕公网回来。`/internal/cred` 返回的 `base_url` 应是这个**内网网关地址**，不是公网 `app.example.com`。详见 §C.3。

### A.5 CORS / Cookie 不冲突

- 母系统现有 cookie 仅 OAuth 流程用，全部 `Path=/`、`SameSite=Lax`，名字是 oauth state/nonce/verifier 系列（`auth_*_oauth.go`）。image-studio 的会话 cookie 设 `Path=/image-studio`，**路径作用域隔离 + 名字不重叠，不冲突**。
- CORS（`middleware/cors.go`，全局挂载）按 `cfg.CORS.AllowedOrigins` 白名单。同源嵌入不带跨域 `Origin`，**CORS 对同源请求无影响**。文档 §7.5 的 CSRF 是 image-studio 自己的事，与母系统无关。

---

## B. 入口 JWT 契约评估（§11.2 / §11.3 / §4.1）

> ✅ **已落地**（见 §D）。下文 B.1 的能力盘点为评估期记录，保留作背景；落地结论以本框 + §D 为准：
> - 私钥走**配置文件路径** `image_studio.jwt_private_key_file`（**非** B.2 早先建议的 DB 设置表，已与用户确认从简起步）。
> - claim 字面值已定：`iss="sub2api"`、`aud="image-studio"`、`sub` = userID 字符串化、`jti` = 16 字节随机 hex、`exp` = `iat + ticket_ttl_seconds`（默认 60s）。
> - jti：**母系统只塞、不兜底**；防重放由 image-studio 验签侧决定（§B.5）。
> - 公钥分发：部署方从私钥导出 `.pub` 后下发 image-studio（无自动分发端点）。

### B.1 现有能力盘点

| 能力 | 现状 | 代码 |
|---|---|---|
| JWT 库 | `github.com/golang-jwt/jwt/v5 v5.2.2` 已在用 | `backend/go.mod:18` |
| RS256 **签名** | 已有样例（Vertex service-account token 交换） | `vertex_service_account.go:226`（`jwt.NewWithClaims(SigningMethodRS256)` + `ParseRSAPrivateKeyFromPEM` + `SignedString`） |
| RS256 **验签** | 已有样例（OIDC 登录） | `auth_oidc_oauth.go:969`，放行算法 `RS256/ES256/PS256`（`:1007`） |
| 会话 JWT | 现用 **HS256**（HMAC 共享密钥），非 RS256 | `auth_service.go:1159`，claims `auth_service.go:57` |
| 机器调用方鉴权 | Admin API Key（`x-api-key` 常量时间比较） | `admin_auth.go:48/132`，密钥 `GetAdminAPIKey`，设置键 `domain_constants.go:279` |
| Redis | 已有（可做 jti 一次性防重放） | 全局基础设施 |

**关键缺口**：母系统**没有**应用级 RSA 密钥对——`ParseRSAPrivateKeyFromPEM` 唯一调用点是「按上游账号存的 Vertex 私钥」（`vertex_service_account.go:230`），不是全局应用密钥。也**没有**任何「短期 ticket 签发端点」。

### B.2 需新增：RSA 密钥对 + 签发路径

1. **生成一对 RSA 密钥**（RS256）。私钥留母系统，公钥给 image-studio。
2. **私钥存放**：两条现成通道任选——
   - 配置文件/env（`config.go` 的 Viper 体系，参考 `JWTConfig` at `config.go:1178`）；或
   - DB 设置表（`SettingService` 的 `GetValue/SetValue` KV 机制，和 `admin_api_key` 同款，`setting_service.go`）。
   - **实际落地：走配置文件路径 `image_studio.jwt_private_key_file`**（启动期 `os.ReadFile` 读 PEM、`NewImageStudioService` 解析、失败 fail-fast）。未采用 DB 设置表——与用户确认从简起步，需热轮换时再迁。公钥由部署方从私钥导出后下发 image-studio。
3. **签名**：直接复刻 `vertex_service_account.go:226` 的 `jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(privKey)`。

### B.3 claim 约定（对齐文档 §4.1）——注意 userID 是 int64

母系统 userID 类型是 **`int64`**（`auth_service.go:57` `JWTClaims.UserID int64`，`auth_subject.go:8` `AuthSubject.UserID int64`）。文档示例写的是 `"sub": "user-12345"`，落地时把 int64 **字符串化**放进 `sub` 即可（image-studio 的 store 列是 TEXT，兼容）。

母系统签发的 ticket claim：

```json
{
  "sub": "12345",            // userID（int64 转字符串）
  "iss": "sub2api",          // 与 image-studio 校验值对齐，建议常量化
  "aud": "image-studio",
  "exp": <iat + 60>,         // ≤60s
  "iat": <now>,
  "jti": "<uuid>"            // 一次性所需，见 B.5
}
```

> ✅ **已落地**：`iss="sub2api"`、`aud="image-studio"`（均可经配置 `image_studio.jwt_issuer` / `jwt_audience` 调整，默认即此值）。image-studio 验签时须逐字校验这两个值。公钥分发：由部署方从 `image_studio.jwt_private_key_file` 导出公钥后下发 image-studio（母系统未提供公钥端点）。

### B.4 ticket 注入方式（§11.3）

推荐 **URL query `?ticket=<JWT>`**（与文档 §4.6 一致）：母系统前端在打开 image-studio iframe/跳转前，用已登录会话调一个新端点拿 ticket，拼到 URL。image-studio 验签换 cookie 后**立即清除 URL 上的 ticket**（文档已写）。postMessage 亦可，但 URL query 更简单且无跨窗口 origin 校验负担。这是母系统**前端 + 一个后端端点**的新增工作。

### B.5 「一次性」的代价（决策点）

文档要求入口 JWT「一次性」。纯靠 `exp≤60s` 只能把重放窗口压到 60 秒，**不是真正一次性**。真一次性需要 `jti` + Redis 黑名单（验签后标记 jti 已用，TTL=60s）。

- 母系统有 Redis，做得到，但**一次性校验逻辑在 image-studio 侧验签时执行**（它持公钥），所以更可能是 **image-studio 自己用它的存储记 jti**。母系统这边只需在 claim 里塞 `jti`。
- 若可接受「短 exp 而非严格一次性」，母系统侧零额外工作。
- **✅ 已定稿（与用户确认）**：母系统负责塞 `jti` + 短 exp（每张 ticket 含 16 字节随机 hex `jti`），**不在母系统侧记录防重放**；是否做严格一次性由 image-studio 验签侧用自己的存储决定。

---

## C. `/internal/cred` 接口契约评估（§11.4 / §4.2）

### C.1 凭证接口形状：两段式（列候选 / 取明文）——已按选定策略定稿

> 选定策略（§C.4）：**用户自选 + 记住 + 兜底自建**。这把单一 `/internal/cred` 拆成**两个内部操作**，因为 image-studio 后端持的是它自己的会话 cookie，**不能**以用户身份直接调母系统的 `/api/v1/api-keys` 或 `/groups/available`——候选数据必须走 service-to-service 的 `/internal/*` 通道按 uid 取。

> 🚨 **响应信封（最关键，对接必读）**：母系统**所有** HTTP API（含 `/internal/*`）统一用信封格式 `{"code":0,"message":"success","data":{...}}`（`internal/pkg/response/response.go` 的 `Response` / `Success`）。**本文档下面所有 JSON 示例画的都是 `data` 字段的内容，外面还套着这层信封。** 消费方解析时**必须先剥信封再取 `data`**：
> ```json
> { "code": 0, "message": "success", "data": { "keys": [ ... ], "can_create": true, "image_group_id": 4, "image_group_name": "chatgpt" } }
> ```
> - 成功：HTTP 200 + `code == 0`，真正的 payload 在 `data` 里。
> - 失败：HTTP 状态码非 2xx（如 400/401/404），`code` 为非 0；越权/不可出图的取明文 → 404（见 §D 末）。
> - ⚠️ **陷阱**：若直接把整个信封 `Unmarshal` 进裸结构体（`{keys, can_create, image_group_id}`），字段一个都对不上，会**静默得到全零值**（空 keys + `can_create=false` + `image_group_id=null`），在 UI 上伪装成「未开启图片功能」。这正是首次联调踩的坑——文档此前漏标信封、`resolver_test.go` 也用裸 JSON 复刻了同一错误假设，故一直未暴露。

**操作 1 — 列候选（`GET /internal/cred/keys?uid=<userID>`）**
返回该用户**当前能出图**的 key 列表，供 image-studio 渲染选择器。**不含明文 key**（以下为 `data` 字段内容，外层信封见本节开头 🚨）：

```json
{
  "keys": [
    { "key_id": 123, "name": "我的图片key", "quota": 10.0, "quota_used": 2.3, "expires_at": 1788192000, "group_id": 7, "group_name": "图片专用" }
  ],
  "can_create": true,
  "image_group_id": 7,            // 兜底自建时该绑哪个 group；动态发现，见下
  "image_group_name": "图片专用"   // 该兜底 group 的名称（供引导文案显示）
}
```
- 数据源：`ListByUserID`（`api_key_repo.go:306`）返回含 group 的完整结构，按 §C.4 三道门槛（平台=OpenAI、group `allow_image_generation=true`、active 且未过期且有剩余额度）过滤。`expires_at` 为 Unix 秒（`null` = 永不过期）。
- **明文 key 不出现在这一步**，降低泄露面。
- **`image_group_id`/`can_create` 是动态发现的**，不来自配置：每次请求即时查 `GetAvailableGroups(uid)`（`api_key_service.go:782`，已按该用户可绑权限过滤——专属/订阅分组无权则不返回），取第一个满足出图条件（openai + 开了作图 + 活跃）的分组。分组删改重建均不受影响；用户无可用分组（系统没有 or 没权限）时 `can_create=false`，前端统一提示「您当前没有可用的绘图分组（或没有权限），请联系客服处理」。

**操作 2 — 取明文（`GET /internal/cred?uid=<userID>&key_id=<id>`）**
按用户选定的 `key_id` 返回真正凭证，返回前**再校验一次**归属（key 属于该 uid）、未过期、有剩余额度、仍满足出图条件（以下为 `data` 字段内容，外层信封见本节开头 🚨）：

```json
{ "api_key": "sk-...", "base_url": "...", "model": "gpt-image-2" }
```
- **api_key**：明文存储（`api_key.go` schema `field.String("key")`，非哈希），按 key_id + uid 读出：`ListByUserID`（`api_key_repo.go:306`）/`GetByKey`（`:106`）。
- **base_url**：见 §C.3。**model**：见 §C.6。
- image-studio 自己存 `userID → 选定 key_id`：有额度没过期就直接走操作 2、不再问用户（满足"记住"）。

**兜底自建**：操作 1 返回空 + `can_create=true` 时，image-studio 引导用户回母系统建 key（或母系统提供一个"在图片专用 group 下建 key"的便捷入口）。母系统**不替用户自动铸 key**——由用户掌握预算（这正是选定策略与原推荐 (a) 的区别，见 §C.4）。

### C.2 鉴权：复用 Admin API Key 还是新建内部密钥？（决策点）

文档 §7.2 要求 `/internal/cred` 必须 service-to-service 鉴权（`X-Internal-Secret` 或 mTLS），否则「拿 userID 即可查他人 api-key」。

母系统现成的机器鉴权是 **Admin API Key**（`x-api-key` → `validateAdminAPIKey` → 常量时间比较，`admin_auth.go:48/132`）。但它**会冒充首个 admin 用户**，权限过大，不宜直接复用到一个只该「按 uid 查 key」的内部端点。

**已落地**：新增专用内部密钥中间件 `NewInternalSecretMiddleware`（`internal/server/middleware/internal_secret_auth.go`），读 `X-Internal-Secret` 头、常量时间比较（`subtle.ConstantTimeCompare`），密钥来源是配置 `image_studio.internal_secret`（≥32 字节，`Validate` 强制）。作用域仅限 `/internal/*`，**不冒充 admin、不碰用户会话**。未配置密钥时一律 401（不裸奔）。密钥走配置而非 DB 设置表——起步从简，后续需热轮换再迁 DB。

> ⚠️ 部署提醒（**未由代码强制，nginx 必做**）：`/internal/*` **绝不能经公网反代暴露**。它要么走容器内网/localhost，要么在 nginx 层显式拒绝外部访问（不为它配 `location`，或显式 `deny`）。代码侧已把 `/internal/` 加入 `shouldBypassEmbeddedFrontend`（`embed_on.go`，避免被内嵌 SPA 兜底吞掉），但**可达性控制是 nginx 职责**——带密钥即可查任意用户的 key，上线前务必在 nginx 挡住。

### C.3 base_url 返回什么？（决策点）

如 §A.4，image-studio 后端是以网关客户端身份调 `/v1/images/generations`。母系统：

- **配置侧**：现有唯一的对外 URL 设置是 `server.frontend_url`（`config.go:553`，默认空），且**没有**专门的「公开 API 站点 base url」设置项；`server.host/port` 只是绑定地址。
- **建议返回内网网关地址**：`base_url = "http://<母系统内网地址>:8080/v1"`（image-studio 与母系统同机/同网时）。这样省一次公网 TLS 往返，也避免依赖 `frontend_url` 是否配置。
- 若必须走公网，则 `base_url = frontend_url + "/v1"`，但要求 `frontend_url` 已正确配置。

> ✅ **已落地**：`base_url` 由配置项 `image_studio.gateway_base_url` 直接给出（默认 `http://localhost:8080/v1`），`/internal/cred` 原样返回。部署方按 image-studio 与母系统的网络拓扑填写（同机/同 compose 网络填内网地址）。开发期母系统二进制监听 8080，故填 8080；生产为前端内嵌同进程、同一 origin，跟着环境填那个统一地址即可。

### C.4 凭证选取策略：用户自选 + 记住 + 兜底自建（已定稿）

**最终决策**：image-studio 让用户从自己**能出图的** key 里挑一把；只要这把 key 有额度、没过期就记住、不再追问；没有可用的就引导用户自己去母系统建。理由:凭证全程在系统内同源流转(image-studio 后端内存,浏览器不持有),泄露面≈信任自部署子进程,**不需要**像聊天广场那样强制铸临时限额 key;同时让用户自己掌控预算。

**"能出图"的三道门槛**(候选过滤的依据,均已核实):

1. **平台门槛**：`/v1/images/generations` 是 **OpenAI-only**（`getGroupPlatform != PlatformOpenAI` 直接 404，`routes/gateway.go:92/104/159/171`；取值逻辑 `:219`）。
2. **分组放行门槛**：`GroupAllowsImageGeneration`（`image_generation_intent.go:22`）——**未分组的 key 放行；已分组的 key 必须其 group `allow_image_generation=true`**（否则报 "Image generation is not enabled for this group"）。图片定价/开关挂在 Group 上（`group.go:29`、`group_service.go:52`：`allow_image_generation`、`image_rate_multiplier`、`image_price_1k/2k/4k`）。
3. **渠道映射**：`gpt-image-2` 只是默认字符串（`openai_images.go:454`，仅校验 `gpt-image-` 前缀 `:457`），实际靠账号映射 `account.GetMappedModel` 解析到真实上游图片模型（`openai_images.go:569-575`）。

**好消息:前端有足够数据自行筛选。** 用户侧 `GET /api/v1/groups/available`(`api_key_handler.go:275`)返回的 `dto.Group` **同时暴露 `platform`(`dto/types.go:95`)与 `allow_image_generation`(`:106`)**——门槛 1、2 前端可直接判断。门槛 3(渠道映射)是 admin 配置,前端看不到,需靠下面的部署前提兜住。

> ⚠️ **部署前提(admin 必做,否则"兜底自建"形同虚设)**：必须预先存在**至少一个**"OpenAI 平台 + `allow_image_generation=true` + 账号映射好 `gpt-image-*`"的 group。否则:用户即便去建 key,绑不到合规 group,建出来照样不能出图——这不是用户能解决的,是 admin 的前置配置。
>
> ✅ **已落地(动态发现,非配置写死)**：操作 1 返回的 `image_group_id`/`image_group_name` 由后端**每次请求即时查** `GetAvailableGroups(uid)` 后过滤(openai + 开了作图 + 用户可绑)得到第一个满足的分组——分组删改重建、ID 变化均不受影响,也无需任何 `group_id` 配置项。若该用户没有可用的合规分组(系统里没有、或有但他无权限/未订阅),`can_create=false`,image-studio 统一提示"您当前没有可用的绘图分组(或没有权限),请联系客服处理"。

> **与原推荐 (a) 的区别**：早先建议由 `/internal/cred` 自动现铸专用限额 key。现改为**用户自选/自建**——母系统侧只读不铸,接口更简单(§C.1 两段式),且预算由用户掌控。安全性不降:同源流转、明文仅 image-studio 内存持有、不落盘(文档 §7.8)。

### C.5 缓存 TTL（§11.5）——母系统无需改造

image-studio 侧 30-60s TTL 缓存（文档 §4.2）。母系统每次被调时返回当时数据即可，无状态。用户改 key 后的生效延迟 = image-studio 的 TTL，母系统不参与。

### C.6 渠道模型名（§11.6）——✅ 已实现可配

`GET /internal/cred` 返回的 `model` 取自配置 `image_studio.image_model`（默认 `gpt-image-2`），**非写死**。部署方按自己渠道映射填即可，替换文档 §4.2 里 image-studio 写死的 `gpt-image-2` / `gpt-5.4-mini`。

### C.7 计费与配额——天然闭环，无需额外工作

因为 image-studio 用的是母系统真实 api key 调网关，所有出图请求**自动走母系统的 token 计量、配额、限流、request_log**。无需为 image-studio 单建计费。按选定策略（§C.4），用户挑哪把 key 出图，预算就记在那把 key 上——用户自己掌控图片工作台的花费上限。

---

## D. 母系统侧落地状态（已实现 ✅）

> 状态：母系统（sub2api）侧前后端**全部已落地并合并到分支 `feat/image-studio-integration`**（5 个 commit）。
> 后端端点已在真实环境用真实用户/分组 curl 验证通过（开权限前拿不到 key → 开权限后拿到 key 的完整剧本）。
> 下面把契约里「待母系统新增」逐条回填成**实际实现的端点 / 字段 / 配置项**，供 image-studio 对接。

| # | 项目 | 状态 | 实际落地 |
|---|---|---|---|
| D1 | nginx 加 `location /image-studio/`（排兜底前，SSE 关 `proxy_buffering`，内网拒 `/internal/`） | ⏳ 部署期 | 属阶段 5 运维动作，非代码。代码侧已把 `/internal/` 加入 `shouldBypassEmbeddedFrontend`（`embed_on.go`），避免内嵌 SPA 兜底吞掉 `/internal/cred`。 |
| D2 | 应用级 RSA 密钥对 | ✅ | 私钥走**配置文件路径**（非 DB 设置表，已与用户确认从简起步）：`image_studio.jwt_private_key_file`，启动期 `os.ReadFile` 读入 PEM、`NewImageStudioService` 解析、解析失败 fail-fast。公钥由部署方从私钥导出后下发 image-studio。 |
| D3 | 入口 ticket 签发端点 | ✅ | `GET /api/v1/image-studio/ticket`（已登录用户，走 `jwtAuth`）。RS256 签 `{sub,iss,aud,iat,exp,jti}`；`sub` = userID 的**字符串化 int64**；TTL = `ticket_ttl_seconds`（默认 60s）。返回 `data` 为 `{ "ticket": "<jwt>", "expires_at": <unix秒> }`（外层带统一信封 🚨；此端点由母系统前端消费、走 apiClient 自动剥壳）。功能未启用时返回 404 `Image studio is not enabled`。 |
| D4 | 母系统前端入口 | ✅ | **整页跳转**（非 iframe，已与用户确认）：独立全屏中转页 `ImageStudioView.vue`（不套母系统框架，仅 loading/友好提示），取 ticket 后 `window.location.href = "/image-studio/?ticket=<jwt>"`。侧栏入口按 `image_studio_enabled` 公开 flag 显隐（opt-in，注入 `window.__APP_CONFIG__`）。 |
| D5 | 两段式内部端点 | ✅ | `GET /internal/cred/keys?uid=<userID>`（列候选，**不含明文**）+ `GET /internal/cred?uid=<userID>&key_id=<id>`（取明文）。均由 `X-Internal-Secret` 头保护。返回结构见 §C.1（**注意候选字段已扩展**，见下方「⚠️ 契约更新」）。 |
| D6 | admin 预置图片专用 group | ⏳ 部署期 | 仍是部署前提（§C.4）。但**已无需把 group_id 写进配置**——见下方「⚠️ 契约更新」：兜底 group 改为运行时动态发现。 |
| D7 | 内部密钥 | ✅ | `image_studio.internal_secret`（配置文件，非 DB；≥32 字节，`Validate` 强制）。中间件 `NewInternalSecretMiddleware` 用 `subtle.ConstantTimeCompare` 校验 `X-Internal-Secret` 头。 |
| D8 | jti 一次性防重放 | ✅（母系统侧）| 已与用户确认：**母系统只塞 jti，不兜底**。每张 ticket 含随机 16 字节 hex `jti`；是否记录防重放由 image-studio 验签侧用自己的存储决定。 |

### ⚠️ 契约更新（image-studio 必读，与原 §C.1 有两处差异）

**1. 兜底 group 改为「动态发现」，不再有 `image_group_id` 配置项。**
原设计是把图片专用 group 的 ID 写进母系统配置。已废弃——理由：分组随时可能删改重建，ID 写死会导致每次都要查库改配置重启。

现在 `GET /internal/cred/keys` 在响应时**即时调 `GetAvailableGroups(userID)`**，从「该用户当前可绑定的活跃分组」里过滤出第一个满足出图条件（openai 平台 + `allow_image_generation=true`）的分组作为兜底目标。后果：
- 分组删改重建、ID 变化，母系统**无需任何改动**。
- 该过滤天然尊重**专属/订阅分组的授权**：用户无权绑的分组不会出现。所以「系统里没有这种分组」和「有但该用户没权限/没订阅」两种情况**合并**为同一个 `can_create=false`，image-studio 文案统一为「您当前没有可用的绘图分组（或没有权限），请联系客服处理」。

**2. 列候选返回结构（实际字段，替换 §C.1 的示意）：**
> 以下为 `data` 字段内容，外层统一信封 `{"code":0,"message":"success","data":{...}}` 见 §C.1 开头 🚨。
```json
{
  "keys": [
    {
      "key_id": 2,
      "name": "api-key-6-1",
      "quota": 5,            // 0 = 无限
      "quota_used": 0,
      "expires_at": null,    // Unix 秒；null = 永不过期
      "group_id": 4,
      "group_name": "chatgpt"
    }
  ],
  "can_create": true,
  "image_group_id": 4,        // 动态发现的兜底 group（>0 才有意义）
  "image_group_name": "chatgpt"
}
```
取明文 `GET /internal/cred?uid=&key_id=` 返回（同样是 `data` 字段内容，外层带信封）：
```json
{ "api_key": "sk-...", "base_url": "http://<网关内网地址>/v1", "model": "gpt-image-2" }
```
越权（A 用 B 的 key_id）或 key 已不满足出图条件 → **404**（信封 `code` 非 0）。

### 母系统 `image_studio` 配置块（部署方填）

```yaml
image_studio:
  enabled: true
  jwt_private_key_file: "data/image_studio_private.pem"  # RS256 私钥 PEM；公钥导出给 image-studio
  jwt_issuer: "sub2api"                                   # = ticket 的 iss，image-studio 须逐字校验
  jwt_audience: "image-studio"                            # = ticket 的 aud
  ticket_ttl_seconds: 60
  internal_secret: "<≥32 字节随机串>"                       # X-Internal-Secret 共享密钥
  gateway_base_url: "http://localhost:8080/v1"            # /internal/cred 返回的 base_url（内网网关地址）
  image_model: "gpt-image-2"                              # /internal/cred 返回的 model
```
（**无 `image_group_id`**——已改为动态发现。）

**注意**：上述全部是**新增**，**不触碰**母系统现有鉴权/网关/计费链路，向后兼容。image-studio 对母系统而言只是「一个持用户 key 的网关客户端 + 两个新内部端点的调用方」。

---

## E. 决策点最终状态（回填给 §11）

1. **§C.4 凭证选取策略**：✅ 定稿并实现——**用户自选 + 记住 + 兜底自建**（母系统只读不铸，两段式接口）。兜底 group 动态发现（见 §D「契约更新 1」），不写配置。遗留部署前提：admin 预置至少一个图片专用 group（D6）。
2. **§C.3 base_url**：✅ 定稿——`/internal/cred` 返回 `gateway_base_url` 配置值（开发期 `http://localhost:8080/v1`，生产期因前后端内嵌同进程，填那个统一 origin 的 `/v1`）。
3. **§B.5 一次性 jti**：✅ 定稿——母系统塞 `jti` + 短 exp（默认 60s），**不兜底防重放**；严格一次性由 image-studio 验签侧用自己存储记 jti。
4. **§B.3 claim 字面值**：✅ 定稿——`iss="sub2api"`、`aud="image-studio"`（均可配，见配置块）；`sub` = userID 字符串化。公钥分发方式：部署方从 `jwt_private_key_file` 导出公钥，带外下发给 image-studio。
5. **§C.2 内部鉴权**：✅ 定稿——专用 `X-Internal-Secret`（配置 `internal_secret`），非复用 admin api key。
6. **§C.6 model**：✅ 定稿——做成可配返回值 `image_model`（默认 `gpt-image-2`），按部署方渠道映射填。

> **仍需 image-studio 侧确认的对接细节**（非母系统阻塞项）：
> - image-studio 后端能否访问母系统 `gateway_base_url`（同 compose 网络/同机内网）。
> - image-studio 用 RS256 公钥验签时，逐字校验 `iss="sub2api"`、`aud="image-studio"`。
> - image-studio 自行决定是否用 `jti` 做严格一次性防重放。
> - nginx 同源反代落地（D1）+ admin 预置图片 group（D6）是上线前两个部署动作。

---

## F. 对文档本身的一处订正

1. **§4.1 假设母系统会话是「JWT 全程 Bearer / 可签 RS256」**：母系统现有会话 JWT 是 **HS256**，且**无现成应用级 RSA 密钥**。RS256 能力（库 + 签/验样例）齐全，但密钥对和签发端点需新建（§B）。不影响契约可行性，只是工作量需计入。

> 说明：§9 用 nginx 举例与母系统实际部署一致（前置就是 nginx），无需订正。
