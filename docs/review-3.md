# `multi-tenant-redesign.md` Review 3

## 结论

第三轮只复核一个问题：第二轮指出的 P1“子路径 `/image-studio` 的浏览器外部路径模型未闭合”是否已经被消掉。

结论是：**已消掉。**

当前版本的计划已经把下面几件事统一到了同一套规则里：

- 浏览器对外路径统一带 `/image-studio` 前缀，见 `docs/multi-tenant-redesign.md:10-28`
- 后端内部 `ServeMux` 注册路径保持裸 `/auth`、`/api`、`/v1`，见 `docs/multi-tenant-redesign.md:19-24`
- 入口换会话路径、任务接口、凭证选择器接口，都明确区分了“浏览器对外路径”和“后端内部注册路径”，见 `docs/multi-tenant-redesign.md:138-146`、`216-225`、`263-266`、`282-316`
- 图片文件路径和兼容 `/v1/images/*` 返回体里的 `url` 字段，也补上了“必须返回外部可见 `/image-studio/v1/...` 路径”的约束，见 `docs/multi-tenant-redesign.md:242-244`、`250-254`、`305-318`
- cookie 名也已经收口成单一版本 `studio_sid`，见 `docs/multi-tenant-redesign.md:58`、`140`、`509`

结合母系统在 [comments-from-mother.md](/home/chris/projects/ChatGpt-Image-Studio/docs/comments-from-mother.md:1) 里的确认：同源反代可行、nginx 前缀剥离方案成立、`/internal/cred` 两段式可补、RS256 ticket 可补、图片专用 group 前置条件明确，我这轮没有再看到新的 P1 及以上阻断项。

## Findings

### 1. [已关闭] Review 2 的 P1 已被计划文档收口

第二轮的阻断点是“浏览器以 `/image-studio/*` 访问，但文档里仍混着写裸 `/auth`、`/api`、`/v1`，会导致前后端各做各的”。这次已经通过 §0.1 总约束和各章节的路径双写说明收掉。

尤其关键的是图片 URL 这条也补上了：不仅前端基址统一为 `webConfig.apiUrl='/image-studio'`，还明确要求后端在兼容 `/v1/images/*` 的返回体里返回对外路径，而不是内部裸 `/v1/files/...`。这一步是必要的，否则同源子路径下图片仍会跑偏。

### 2. [非阻断] 还有两项实现前最好先定，但不再属于 P1

1. 浏览器历史模式二选一还留在文档里，见 `docs/multi-tenant-redesign.md:318-321`。建议直接采用推荐值：**嵌入版强制 `server` 历史模式，禁用 browser 模式**。这能最稳地避免同浏览器切用户时串本地缓存。
2. `/v1` 返回图片 URL 的外部前缀获取方式，文档给了两种落地法：`X-Forwarded-Prefix` 或 `[server] public_base_path`，见 `docs/multi-tenant-redesign.md:254`、`352-363`。实现时二选一即可，建议优先 `public_base_path`，更稳定、测试也更容易写。

这两项我认为是实施细节选择，不再构成计划级阻断。

## Go / No-Go

**决定：Go**

理由很简单：母系统约束已经确认，第二轮唯一 P1 已在计划文档里消掉，当前版本已经足够作为实现蓝图启动开发。

如果要把风险再压低一点，建议真正开工前先做两件小事：

1. 在文档里把“嵌入版强制 `server` 历史模式”改成最终定稿，而不是保留二选一表述。
2. 在实现任务里明确写一条：“兼容 `/v1/images/*` 的图片 URL 回写，按 `public_base_path='/image-studio'` 生成外部路径”。

但即使不先补这两句，我也不认为现在还需要继续 `No-Go`。
