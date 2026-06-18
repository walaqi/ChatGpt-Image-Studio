# `multi-tenant-redesign.md` Review 2

## 结论

第二轮看下来，v2 计划已经实质性吸收了第一轮 review 和母系统 `comments-from-mother.md` 的关键确认。之前最危险的 4 个阻断项里，任务隔离、异步 userID 透传、图片文件访问模型、导入接口收紧，都已经在文档里被明确设计到了。

但目前我仍然认为存在 **1 个 P1 级问题**，所以本轮结论是：

**No-Go**

原因不是母系统契约没拿到，也不是总体方向错了，而是当前计划里“同源子路径 `/image-studio/*` 的浏览器外部路径”和“后端内部原始路由 `/api` / `/v1` / `/auth/session`”还没有完全闭合。一旦按现在文档直接实施，极大概率会出现：

- cookie 已经下发成功，但前端 API 请求仍然打到母系统根路径而不是 image-studio；
- 图片 URL 漏掉 `/image-studio` 前缀，`<img src>` 和历史图片加载直接 404；
- 入口换会话时，前端调用的路径和后端真实挂载路径不一致。

这会让同源 cookie 模型在浏览器侧整体失效，属于上线前必须消掉的 P1。

## P1 Findings

### 1. [P1] 子路径前缀模型没有定死，浏览器访问路径与后端路由设计仍然前后不一致

v2 文档已经正确确定了部署形态是“母系统域下 `/image-studio/*` + nginx 剥前缀转发”，见 `docs/multi-tenant-redesign.md:21-23`、`docs/multi-tenant-redesign.md:445-446`。母系统回复也确认了这一点是可行的，见 `docs/comments-from-mother.md:A.1-A.3`。

但计划内部对“浏览器实际访问哪个 URL”仍然不一致：

- 目标流里写的是前端调用 `POST /image-studio/auth/session`，见 `docs/multi-tenant-redesign.md:35`。
- 实施清单里又写成新增后端路由 `POST /auth/session`，见 `docs/multi-tenant-redesign.md:119-124`，前端换会话时也写的是 `POST /auth/session`，见 `docs/multi-tenant-redesign.md:252`。
- 凭证选择器 API 一处写 `GET /api/image/credential/keys`、`PUT /api/image/credential/current`，见 `docs/multi-tenant-redesign.md:195-197`；另一处又写成 `GET /api/image/credentials/keys`、`PUT /api/image/credentials/selection`，见 `docs/multi-tenant-redesign.md:259-260`。
- 图片文件目标 URL 仍写成 `/v1/files/image/<userID>/<filename>`，见 `docs/multi-tenant-redesign.md:215`、`366`，但没有说明浏览器侧最终应看到的是 `/image-studio/v1/files/image/...`，还是后端要自己补前缀。

这不是文案小问题，而是会直接落到前端和网关实现上。当前代码明确依赖“浏览器里访问的 URL 就是 `webConfig.apiUrl + /api...` 或 `+ /v1...`”：

- axios 基址来自 `webConfig.apiUrl`，见 [web/src/lib/request.ts](/home/chris/projects/ChatGpt-Image-Studio/web/src/lib/request.ts:29)。
- 图片生成直接请求 `/v1/images/generations`，见 [web/src/lib/api.ts](/home/chris/projects/ChatGpt-Image-Studio/web/src/lib/api.ts:825)。
- SSE 直接请求 `${apiUrl}/api/image/tasks/stream`，见 [web/src/lib/api.ts](/home/chris/projects/ChatGpt-Image-Studio/web/src/lib/api.ts:909)。
- 图片展示会把服务端返回的相对 URL 直接拼到 `apiUrl` 上，见 [web/src/app/image/view-utils.ts](/home/chris/projects/ChatGpt-Image-Studio/web/src/app/image/view-utils.ts:12) 和 [web/src/store/image-conversations.ts](/home/chris/projects/ChatGpt-Image-Studio/web/src/store/image-conversations.ts:229)。
- 后端当前生成的图片 URL 也仍然是 `/v1/files/image/...`，见 [backend/api/files.go](/home/chris/projects/ChatGpt-Image-Studio/backend/api/files.go:52) 和 [backend/api/image.go](/home/chris/projects/ChatGpt-Image-Studio/backend/api/image.go:35)。

如果 nginx 采用文档推荐的“剥掉 `/image-studio` 前缀再转发”，那浏览器对外可访问的所有 image-studio API、SSE、图片文件 URL 都必须是：

- `/image-studio/auth/session`
- `/image-studio/api/...`
- `/image-studio/v1/...`

而不是裸 `/auth/session`、`/api/...`、`/v1/...`。

否则这些请求会直接落到母系统根路径，被母系统自己的 `location /` 或 SPA fallback 接住。母系统回复已经明确指出它的内嵌 SPA 会吞掉未被反代截走的 `/image-studio/*` 之外路径，见 `docs/comments-from-mother.md:A.2`。换句话说，当前计划如果不把“浏览器外部 URL 规则”统一写死，前后端会非常容易各做各的，最终表现为登录能进、静态页能开，但 API、图片、SSE 到处串路由。

**建议修正**

在计划文档里把这一条明确成单一约束，不要再允许两种解释并存：

1. 浏览器可见的 image-studio 根路径固定为 `/image-studio`。
2. 前端一律以 `webConfig.apiUrl = '/image-studio'` 为基址。
3. 所有浏览器请求统一走：
   - `POST /image-studio/auth/session`
   - `GET|POST /image-studio/api/...`
   - `GET|POST /image-studio/v1/...`
4. 后端内部 `http.ServeMux` 仍保持注册裸路径 `/auth/session`、`/api/...`、`/v1/...`，由 nginx 剥前缀后转发。
5. 后端所有返回给浏览器的相对 URL 也必须带外部前缀，或者统一由前端用 `apiUrl='/image-studio'` 去拼接，且文档只能保留一种方案。
6. 文档里把 `credential/keys` vs `credentials/keys`、`studio_sid` vs `studio_session` 这种接口/命名分裂一并收口，否则实施阶段会继续漂移。

在这条没定死前，我不建议开工。

## 其他观察

除上面的 P1 外，我这轮没有再看到新的 P0/P1 级架构阻断项。尤其是下面几点，母系统回复已经足够强：

- 同源反代可行，且母系统明确给出了 nginx 侧约束，见 `docs/comments-from-mother.md:A`。
- RS256 入口 ticket、`jti`、`/internal/cred` 两段式接口、内部密钥，都被母系统确认能补，见 `docs/comments-from-mother.md:B-C-D`。
- “用户自选 key + 记住 + 兜底自建”这条策略，母系统已经按其现有 group / platform / image-generation 约束做了可实现性确认，见 `docs/comments-from-mother.md:C.4`。

所以当前不是“方案整体不可行”，而是“还差最后一条浏览器路径模型收口，收口后可以转 Go”。

## Go / No-Go

**决定：No-Go**

**转 Go 的前置条件只有一条：**

把“浏览器对外 URL 前缀模型”在计划文档里定成唯一版本，并统一以下内容：

- `auth/session` 的外部访问路径
- `/api/*` 与 `/v1/*` 的外部访问路径
- 图片文件 URL 的对外形式
- 前端 `webConfig.apiUrl` 的预期值
- 文档里重复但不一致的接口名和 cookie 名

这条补完后，我会给 **Go**。目前不建议直接按 v2 开始实现。
