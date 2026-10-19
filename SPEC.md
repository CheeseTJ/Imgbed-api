# Spec：图床（Image Bed）

界面方案：`Imgbed-design/variant-a.html`（UPLINK 终端风）

## 0. 项目结构

`Imgbed/` **只是一个本地包裹目录，本身不是仓库**。其下是两个各自独立的仓库
（命名对齐工作区既有约定，见 `Remind/`、`JMReader/`）：

```
Imgbed/                    本地包裹目录，不入库、不构建
├── Imgbed-api/            仓库一：后端 Go + Gin
│   ├── SPEC.md            本文档
│   ├── .env.sh            运行环境变量（shell 格式，含密钥，不入库）
│   ├── .env.docker        由 gen-docker-env.sh 生成，供 compose 使用（不入库）
│   ├── config.yaml        本机运行配置（不入库；模板见 config.yaml.example）
│   ├── cmd/ internal/     源码
│   └── Dockerfile · docker-compose.yml · Makefile · run.sh
└── Imgbed-web/            仓库二：前端 Vue 3 + Vite + TypeScript
    └── src/               源码（api / stores / router / views / styles）
```

两个仓库可以各自克隆、各自部署，**不互相依赖构建产物**；
前端是纯静态站点，只通过 REST API 与后端通信。

本文档因此放在 `Imgbed-api` 仓库内：它描述的是整个系统，
但端点、数据模型、鉴权、存储都归后端仓库管理，前端只消费 REST API（见第 7 节）。

## 1. 目标

给个人及少数熟人使用的图像托管服务。核心链路只有一条：

**拖进浏览器 → 拿到一条永久可用的外链。**

围绕这条链路，必须支持：
- 用 Ucenter 账号登录，图片按用户隔离
- 图片存放在 R2 的目录树里，而不是平铺一堆
- 分享链接可切换 URL / Markdown / HTML / BBCode / CDN 五种格式

## 2. 架构

```
┌──────────────────────────────────────────────────────────────┐
│  浏览器                                                       │
│  静态 SPA（Cloudflare Pages）                                 │
│  ├─ 登录：直连 Ucenter /v1/auth/login  (app=imgbed)         │
│  └─ 业务：Authorization: Bearer <access_token> → 本服务 /api  │
└───────────────┬──────────────────────────┬───────────────────┘
                │                          │
                │ ①鉴权/元数据/上传         │ ②读图（不走本服务）
                ▼                          ▼
┌──────────────────────────────┐   ┌──────────────────────────┐
│  Imgbed-api/  Go+Gin（OCI）   │   │  Cloudflare 边缘          │
│  ├─ JWKS 本地验签（Ed25519）  │   │  img.june-t.top           │
│  ├─ HeatWave(MySQL)：元数据   │   │  缓存命中 → 直接返回       │
│  └─ R2 读写（S3 兼容 API）    │──▶│  缓存未命中 → 回源 R2      │
└──────────┬───────────────────┘   └──────────┬───────────────┘
           ▼                                  ▼
 ┌──────────────────────────┐     ┌──────────────────────────┐
 │  OCI HeatWave (MySQL)    │     │  R2 bucket: image         │
 │  用户 / 目录 / 图片 元数据 │     │  图片二进制                │
 └──────────────────────────┘     └──────────────────────────┘
```

关键分工：
- **上传走本服务**，因为要鉴权、算哈希去重、写元数据。
- **读图不走本服务**，直接由 R2 自定义域名经 CDN 返回，零成本、最低延迟。
- 元数据在 **OCI HeatWave**，图片二进制在 **R2**，两边各管一件事：
  数据库只存指针（`object_key`），R2 只听数据库的指挥。
- 本服务不保存图片二进制，除数据库连接池外**无状态**，可以随时重启。

## 3. 鉴权（接入 Ucenter）

### 3.1 现状结论

Ucenter **不是 OAuth2 / OIDC**。没有 `client_id`、没有 `client_secret`、没有回调白名单、没有 `/authorize`、没有 `/introspect`。

它的模型是：**密码换 JWT + 各业务方拉 JWKS 本地验签**。

### 3.2 前置动作（必须先做）

在 Ucenter Worker 的 `APPS` 环境变量里追加本应用标识，并重新 `wrangler deploy`：

```
APPS = "jmreader,music,cicd,remind,imgbed"
```

这个值会成为 JWT 的 `aud`，本服务必须校验它，否则等于接受了发给别的应用的令牌。

### 3.3 登录流程（前端直连 Ucenter）

前端不经过本服务转发密码：

| 步骤 | 请求 | 说明 |
|---|---|---|
| 登录 | `POST {UCENTER}/v1/auth/login`<br>`{ email, password, app: "imgbed", device_id, device_name, platform }` | 返回 `access_token` + `refresh_token` + `expires_in` + `device_id` |
| 刷新 | `POST {UCENTER}/v1/auth/refresh`<br>`{ refresh_token, app: "imgbed" }` | 401 时单飞刷新，同一时刻只允许一个刷新请求在飞 |
| 登出 | `POST {UCENTER}/v1/auth/logout`<br>`{ refresh_token }` | 之后清空本地存储 |

前端持久化到 `localStorage`：`access_token`、`refresh_token`、`device_id`。

### 3.4 后端验签

参考实现：`Remind/Remind-api/internal/auth/jwks.go` 与 `middleware.go`。

1. 启动时拉取 `{UCENTER}/.well-known/jwks.json`，内存缓存 **1 小时**。
2. 解析 `Authorization: Bearer <token>`，取 JWT header 的 `kid`。
3. **`kid` 未知时强制刷新一次 JWKS**（应对密钥轮换），再次未命中才拒绝。
4. 用 Ed25519 公钥验签。
5. 断言以下内容，任一不满足返回 401：

| 字段 | 期望值 |
|---|---|
| `header.alg` | `EdDSA`（必须显式检查，防算法混淆） |
| `exp` | 未过期 |
| `aud` | `imgbed` |
| `sub` | 非空 → 作为 `owner_id` |

6. 通过后把 `sub` / `email` / `role` 放进 Gin context。

### 3.5 影子用户表

首次见到某个 `sub` 时，用 claims 里的 `email` upsert 进本地 `users` 表，避免每个请求都跨域调 `/v1/me`。

**已知取舍**：本地验签无法即时吊销。用户在 Ucenter 被禁用后，最长 15 分钟（`expires_in` 默认 900 秒）仍可访问。个人场景可接受；若不可接受，在删除等敏感操作前额外调一次 `GET {UCENTER}/v1/me` 复核。

## 4. 存储设计（R2）

### 4.1 前提事实

R2 没有真实目录。控制台的「创建文件夹」写入的是**以 `/` 结尾的 0 字节占位对象**，S3 API 通过 `Delimiter=/` + `CommonPrefixes` 把它呈现为目录树。

因此：**目录树是我们约定出来的，DB 才是权威数据源，R2 key 是它的派生视图。**

### 4.2 Bucket 与 Key 规划

- Bucket：`image`
- 根前缀由 `R2_PREFIX` 控制（留空 = bucket 根；若复用已有 bucket 则填 `image/`）

Key 结构：

```
{R2_PREFIX}u/{owner_id}/{folder_path}/{image_id}.{ext}

例：
u/uc_8f2a.png...   下略
u/uc_8f2a/风景/2026/01H8XK2M9P.jpg
u/uc_8f2a/风景/2026/01H8XK3Q7T.png
u/uc_8f2a/截图/          ← 0 字节占位对象，保证空目录可见
```

- `image_id` 用 **ULID**（单调递增、可排序、无碰撞），不用原始文件名：原始文件名可能含中文、空格、特殊符号，直接进 key 会带来永久性的 URL 编码麻烦。
- 展示名存在 DB 的 `images.filename`，改展示名**不需要动 R2**。
- 改**目录名**才需要迁移 key（见 4.4）。
- **目录名进 key 时会清洗，但保留 Unicode 字母**：中文目录名原样保留（`u/uc_8f2a/风景/2026/...`），
  空格、斜杠、引号、`%` 等换成下划线，连点折叠（消灭 `..`）。
  早先的实现是「非 `[a-zA-Z0-9_.-]` 一律替换」，那会让「风景」和「截图」都塌缩成 `__`，
  与本节选方案 A 的理由（控制台里能看懂目录结构）直接矛盾。改动见 `sanitizeSegment()` 的注释。

### 4.3 目录树的两条路线（已选定 A）

| | A. Key 编码路径（**选定**） | B. Key 用 ULID，树只存 DB |
|---|---|---|
| R2 控制台可读性 | ✅ 能直接看到目录树 | ❌ 一堆 ULID |
| 移动/改名文件 | 需迁移 key | 只改 DB，零 R2 操作 |
| 移动/改名目录 | 需批量迁移 key | 只改 DB，零 R2 操作 |
| 与 S3 生态兼容 | ✅ 可用 `aws s3 sync` 等工具 | ❌ |

选 A 的理由：个人用量下批量迁移的成本可以忽略（改一个 200 张图的目录约消耗 600 次 Class A，免费额度 100 万次/月），而**控制台里能直接看懂目录结构**是真实且长期的价值。

### 4.4 目录重命名 / 移动的执行方式

R2 无原子 rename，因此实现为**异步任务**：

```
1. 标记该目录及子目录下所有 image 为 migrating（此时图库只读该目录）
2. 对每个对象：CopyObject(旧 key → 新 key)
3. 全部成功后 UpdateObjectMetadata + 删除旧 key
4. 更新 DB 中 folder.path 与 image.object_key
5. 清除 migrating 标记
```

- 失败可重入：只处理「新 key 不存在」的对象。
- 期间前端显示进度条，走 `GET /api/v1/jobs/:id` 轮询。
- 目录规模上限：单次迁移超过 5000 个对象时先提示确认。

### 4.5 分发方式

**读图直连 R2 自定义域名**（`https://img.june-t.top/<key>`），不经过本服务。

- CDN 缓存命中 → 不消耗本服务任何资源，也不消耗 R2 Class B。
- 缓存策略：`Cache-Control: public, max-age=31536000, immutable`（key 含 ULID，内容永不变）。
- 上传时设 `Content-Type` 与 `Cache-Control` 为对象元数据，确保 CDN 正确缓存。

**私有图片**（`visibility = private`）：

- 不通过自定义域名暴露（该域名只映射公开前缀，或私有对象单独放 `private/` 前缀且不绑定域名）。
- 走 `GET /api/v1/images/:id/raw`，由本服务验签后流式透传。
- 局限：这样私有图片没有 CDN 加速，且占用本服务带宽。个人用量下可接受。

### 4.6 访问 R2 的两条路径（已决策：走 OCI）

有两条路可以从业务侧读写 R2：从 OCI 的 Go 服务用 S3 兼容 API，或从 Cloudflare Worker 用原生 binding。

| | OCI 服务 → R2（S3 API） | Worker → R2（binding） |
|---|---|---|
| 传输 | 公网 HTTPS + AWS SigV4 签名 | Cloudflare 内部 RPC，不出网 |
| 延迟 | 跨云公网 RTT，视地域 20–200ms | 内部调用，亚毫秒~毫秒级 |
| 凭据 | 需要 Access Key / Secret，自行保管轮换 | binding 免密钥 |
| **Class A / B 计费** | **照常计** | **照常计** |
| R2 出口流量 | $0 | $0 |

**结论：Worker 的 binding 不会省下任何额度**，它只省掉 HTTPS 握手、SigV4 签名和公网跳转。
Class A（写）与 Class B（读）在两条路径下计费完全一致。

**真正需要注意的成本点在 OCI 的出网流量上**：R2 免的是它自己的出口流量；如果图片要**经过** OCI 服务器再转发给用户，
那么 OCI → 用户 这一段是 Oracle 计费的（Always Free 每月含 10 TB 出网，超出按量）。本设计里读图不经过 OCI，
所以 OCI 只在**上传**时产生出网（浏览器 → OCI → R2，每张图一次），量可以忽略。

因此当前选择走 OCI，不额外维护 Worker。什么时候才值得加 Worker：上传量很大、或要让浏览器直传 R2
以绕开「浏览器 → OCI → R2」这个双跳时，可在 CF 侧放一个极薄的 Worker 只做签名转发。

## 5. 数据模型（MySQL / OCI HeatWave）

```sql
-- 从 Ucenter 同步的影子表
CREATE TABLE users (
  id           TEXT PRIMARY KEY,          -- = JWT 的 sub
  email        TEXT NOT NULL,
  role         TEXT NOT NULL DEFAULT 'user',
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);

-- 虚拟目录树
CREATE TABLE folders (
  id         TEXT PRIMARY KEY,
  owner_id   TEXT NOT NULL REFERENCES users(id),
  parent_id  TEXT REFERENCES folders(id),  -- NULL = 根目录
  name       TEXT NOT NULL,
  path       TEXT NOT NULL,                -- 物化路径，如 "风景/2026"
  migrating  INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  UNIQUE(owner_id, parent_id, name)
);

CREATE TABLE images (
  id          TEXT PRIMARY KEY,            -- ULID
  owner_id    TEXT NOT NULL REFERENCES users(id),
  folder_id   TEXT REFERENCES folders(id), -- NULL = 根目录
  object_key  TEXT NOT NULL UNIQUE,
  filename    TEXT NOT NULL,               -- 展示名，可随意改
  ext         TEXT NOT NULL,
  mime        TEXT NOT NULL,
  size        INTEGER NOT NULL,
  width       INTEGER,
  height      INTEGER,
  sha256      TEXT,                        -- 去重 / 秒传
  visibility  TEXT NOT NULL DEFAULT 'public',  -- public | private
  views       INTEGER NOT NULL DEFAULT 0,
  deleted_at  INTEGER,                     -- 软删除（回收站），NULL = 正常
  created_at  INTEGER NOT NULL
);
CREATE INDEX idx_images_list ON images(owner_id, folder_id, created_at DESC);
CREATE UNIQUE INDEX idx_images_dedup ON images(owner_id, sha256) WHERE deleted_at IS NULL;

-- 异步任务（目录迁移）
CREATE TABLE jobs (
  id         TEXT PRIMARY KEY,
  owner_id   TEXT NOT NULL,
  kind       TEXT NOT NULL,                -- folder_migrate
  payload    TEXT NOT NULL,                -- JSON
  total      INTEGER NOT NULL DEFAULT 0,
  done       INTEGER NOT NULL DEFAULT 0,
  status     TEXT NOT NULL,                -- pending | running | done | failed
  error      TEXT,
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL
);
```

`folders.path` 是冗余字段，用于：按前缀查子树（`path LIKE '风景/%'`）、前端直接渲染面包屑、生成 R2 key。改名时事务内同步更新本行与所有子行。

## 6. API 设计

统一前缀 `/api/v1`。除 `/api/health` 外均需 `Authorization: Bearer`。

错误统一为 `{ "code": "...", "message": "..." }`（对齐 Ucenter 的错误格式）。

### 6.1 身份

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/me` | 影子用户信息 + 配额用量 |
| GET | `/api/v1/usage` | `{ used_bytes, total_bytes, image_count, folder_count }` |

### 6.2 目录

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/folders/tree` | 返回整棵树 + 每层图片数 |
| POST | `/api/v1/folders` | `{ parent_id, name }` |
| PATCH | `/api/v1/folders/:id` | `{ name }` → 若目录非空，返回 `{ job_id }` |
| POST | `/api/v1/folders/:id/move` | `{ parent_id }` → 同上 |
| DELETE | `/api/v1/folders/:id` | `?recursive=true` 才允许删除非空目录 |

`GET /folders/tree` 响应：

```json
{ "tree": [
  { "id": "f1", "name": "风景", "path": "风景", "count": 42, "children": [
      { "id": "f2", "name": "2026", "path": "风景/2026", "count": 18, "children": [] }
  ]}
]}
```

### 6.3 图片

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/v1/images/upload` | `multipart/form-data`：`file`、`folder_id?`；返回图片对象 |
| GET | `/api/v1/images` | `?folder_id=&q=&page=&size=&sort=` |
| GET | `/api/v1/images/:id` | 单张详情 |
| GET | `/api/v1/images/:id/raw` | 仅 `private` 图片走这里，流式透传 |
| PATCH | `/api/v1/images/:id` | `{ filename?, folder_id?, visibility? }` |
| DELETE | `/api/v1/images/:id` | 软删除 |
| POST | `/api/v1/images/batch` | `{ ids, action: "move"\|"delete"\|"restore", folder_id? }` |
| GET | `/api/v1/trash` | 回收站列表 |
| DELETE | `/api/v1/trash` | 清空回收站（真正删 R2 对象） |

上传响应：

```json
{
  "id": "01H8XK2M9P...",
  "filename": "sunset-over-harbor.jpg",
  "folder_id": "f2",
  "size": 2526103,
  "width": 4032,
  "height": 3024,
  "mime": "image/jpeg",
  "visibility": "public",
  "url": "https://img.june-t.top/u/uc_8f2a/风景/2026/01H8XK2M9P.jpg",
  "created_at": 1790000000
}
```

`url` 由后端拼好返回，前端不再自己拼域名——域名一旦变更只改后端配置。

### 6.4 任务

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/jobs/:id` | `{ status, total, done, error }` |

### 6.5 健康检查

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/health` | 返回 Ucenter 连通性、R2 连通性、DB 路径（不回显任何密钥值） |

### 6.6 回收站策略

软删除保留 **7 天**：

- `DELETE /api/v1/images/:id` 只写 `deleted_at`，R2 对象与 DB 行都保留。
- 后台定时任务每小时扫一次，把 `deleted_at` 早于 7 天前的记录**物理删除**：先删 R2 对象，再删 DB 行。
- `DELETE /api/v1/trash`（清空回收站）走同一条物理删除路径，忽略保留期。
- 删除目录连带图片时，同样走软删除，不直接物理删除。
- `GET /api/v1/trash` 只返回 7 天内的记录，并给出 `expires_at`。

## 7. 前端设计

技术栈：**Vue 3 + Vite + TypeScript**（与 `Remind-web`、`Ucenter-web` 保持一致），不引入 UI 组件库，样式手写以还原方案 A。

### 7.0 已落地的工程结构（M2）

```
Imgbed-web/
├── vite.config.ts          /api 代理到 127.0.0.1:18080（开发期绕开 CORS）
├── env.d.ts                环境变量与 .vue 的类型声明
└── src/
    ├── api/
    │   ├── ucenter.ts      认证客户端：令牌管理 / 单飞刷新 / 401 自动重试
    │   └── client.ts       业务 API 客户端（指向 Imgbed-api）
    ├── stores/auth.ts      登录态（reactive，不引入 Pinia）
    ├── router/index.ts     路由 + 登录守卫
    ├── components/AppShell.vue
    ├── views/{LoginView,LibraryView}.vue
    ├── styles/main.css     设计令牌 + CRT 叠加 + 无障碍
    └── utils/format.ts
```

关于认证层的两个刻意选择：

1. **刷新令牌必须单飞。** `refresh_token` 是一次性的，并发刷新会被 Ucenter 判为令牌泄露并
   **吊销整条令牌链**。页面上多个请求同时收到 401 是常态，所以 `refreshTokens()` 用共享 Promise
   保证同一时刻只有一个刷新在飞——这不是优化，是正确性要求。
2. **登录态失效用回调注入，不让 `ucenter.ts` 直接 import router。** 否则会形成
   `ucenter → router → stores → ucenter` 的循环依赖。

另外，`prefers-reduced-motion` 下所有入场动画会被压缩为瞬时完成（而非删除）——
终端风的入场动画起始态是 `opacity: 0`，一旦动画没跑起来内容会**永远不可见**。

视觉沿用 `Imgbed-design/variant-a.html`：IBM Plex Mono、`#08090a` 底、`#35ff9e` 荧光绿、CRT 扫描线、等宽表格。

### 7.1 布局（在方案 A 基础上的调整）

```
┌────────────┬─────────────────────────────────────┬──────────────┐
│ 侧栏        │ 顶部：面包屑 风景/2026/  + 搜索       │ inspector    │
│            ├─────────────────────────────────────┤              │
│ 文件夹树     │ 拖拽上传区（落到当前目录）             │ 预览          │
│ ├ 风景      │                                     │ 元数据        │
│ │ ├ 2026   │ 上传队列（ASCII 进度条）              │ 链接格式      │
│ │ └ 街拍    │                                     │ [复制]        │
│ ├ 截图      │ ─────────────────────────────────── │              │
│ └ 未分类    │ ID │ 文件名 │ 尺寸 │ 分辨率 │ 时间    │ [重命名]      │
│            │ ─────────────────────────────────── │ [移动] [删除] │
│ 存储配额     │ （当前目录的图片表格，可多选）         │              │
└────────────┴─────────────────────────────────────┴──────────────┘
```

方案 A 原型里的左栏导航（上传/图库/统计/令牌）收敛为：**图库**（主视图，即上图）、**回收站**、**设置**。

### 7.2 路由

| 路径 | 视图 |
|---|---|
| `/login` | 登录页 |
| `/f/:folderId?` | 图库（`folderId` 省略 = 根目录） |
| `/trash` | 回收站 |
| `/settings` | API Token / 配额 / 账号 |

### 7.3 交互细节

- 顶部 prompt 行复用方案 A 的打字机效果，内容改为**当前路径**：`uplink@edge-01: ~/风景/2026$ ✓ 18 items`
- 拖拽上传时高亮当前目录；拖到左栏某个文件夹名上则上传到该目录。
- 上传队列按方案 A 的 `████░░` 方块渲染，走 `XMLHttpRequest.upload.onprogress`。
- 表格支持多选（`Shift` 区间选、`Cmd/Ctrl` 点选），选中后底部浮出批量操作条。
- 右键图片/目录出上下文菜单（重命名 / 移动 / 复制链接 / 删除）。

### 7.4 链接格式

与方案 A 一致，五种格式，`url` 由后端返回后本地拼接：

| 格式 | 输出 |
|---|---|
| URL | `https://img.june-t.top/<key>` |
| Markdown | `![<filename>](<url>)` |
| HTML | `<img src="<url>" alt="<filename>" loading="lazy">` |
| BBCode | `[img]<url>[/img]` |
| CDN 变换 | `https://img.june-t.top/cdn-cgi/image/w=1600,q=82,f=auto/<key>`（需开启 Cloudflare Image Resizing，见第 11 节） |

## 8. 配置项

`Imgbed-api/config.yaml`（参考 `Remind-api/config.yaml.example` 的结构）：

```yaml
server:
  addr: ":18080"
  # 浏览器直连本服务的来源白名单（CORS）。留空 = 全放行。
  allowed_origins:
    - "https://img.taojun.top"

ucenter:
  base_url: "https://ucenter.june-t.top"
  app: "imgbed"            # 必须与 Ucenter 的 APPS 白名单一致
  jwks_ttl: "1h"

r2:
  account_id: ""
  access_key_id: ""
  secret_access_key: ""      # 建议用环境变量 IMGBED_R2_SECRET_ACCESS_KEY 覆盖
  bucket: "image"            # 已确认新建，不与其他项目共用
  prefix: ""                 # 留空 = bucket 根
  public_base: "https://img.june-t.top"
  private_prefix: "private/" # 私有对象放这里，不绑定自定义域名

limits:
  max_upload_mb: 20
  max_batch: 50
  folder_migrate_warn: 5000  # 超过此数量迁移前需二次确认
  trash_retain_days: 7       # 回收站保留天数，到期物理删除

db:
  # OCI HeatWave（MySQL 兼容）。两种给法二选一：
  #   1) 直接给 dsn
  #   2) 留空 dsn，用下面的分字段拼装（推荐，密码走环境变量）
  dsn: ""
  host: ""                       # 例：xxx.subnet.oci.oraclecloud.com
  port: 3306
  name: "imgbed"
  user: "imgbed"
  password: ""                   # 用 IMGBED_DB_PASSWORD 环境变量覆盖
  tls: true                      # HeatWave 一般要求 TLS
  max_open_conns: 16
  max_idle_conns: 8
  conn_max_lifetime_sec: 180
```

密钥一律走环境变量覆盖（`IMGBED_R2_SECRET_ACCESS_KEY` 等），不落盘。

## 9. 部署

| 组件 | 部署位置 | 对外域名 |
|---|---|---|
| `Imgbed-api/` | Docker 容器（`docker compose up -d`），跑在本机 `instance-20260820-1632`（同 VCN） | `https://imgbed.june-t.top` |
| `Imgbed-web/` | Cloudflare Pages（构建产物纯静态） | `https://img.taojun.top` |
| R2 | bucket `image` | `https://img.june-t.top`（图片直出） |
| 元数据 | OCI HeatWave `10.0.0.226` / 库 `imgbed` | — |

上线前检查清单：

1. ~~Ucenter `APPS` 已追加 `imgbed` 并重新部署~~
   ✅ 2026-09-30 完成。push 触发 GitHub Actions 自动部署，实测 `unknown_app` → `invalid_credentials`，白名单已生效。
2. ~~R2 bucket `image` 已创建，自定义域名已生效并签发证书~~
   ✅ 2026-09-30 完成。`wrangler r2 bucket create image` + `domain add image --domain img.june-t.top`，域名已解析到 Cloudflare 边缘。
3. ~~为 `image` bucket 建一份 R2 凭据（S3 Access Key / Secret，权限 Object Read & Write，限定该 bucket）~~
   ✅ 2026-09-30 完成。已写入 `.env.sh`（`IMGBED_R2_ACCESS_KEY_ID` / `IMGBED_R2_SECRET_ACCESS_KEY`）。
   真实 R2 往返测试通过：上传 → 存在性 → 内容与类型 → **自定义域名直出（含 `Cache-Control: immutable`）** → 删除。
   ⚠️ 注意：控制台里那个 `cfut_` 开头的值是「令牌值」（用于 Cloudflare API），
   **不是** S3 凭据；真正要填的是同一页面下方的「访问密钥 ID」与「机密访问密钥」（纯十六进制）。
4. R2 bucket CORS 无需配置（上传走后端，不存在浏览器直传）
5. HeatWave 已建库与账号，Go 服务能连通（启动失败会打印具体原因）
6. HeatWave 侧已放通 Go 服务的来源 IP / 子网，且 TLS 能正常握手

## 10. 里程碑

| 阶段 | 内容 | 产出 |
|---|---|---|
| M1 | Go 骨架 + Gin 路由 + HeatWave 建表 | `/api/health` 可用 |
| M2 | Ucenter 验签中间件 + 登录页 + 前端令牌管理 | 能用 Ucenter 账号登入 |
| M3 | ~~R2 客户端 + 单图上传 + 根目录图库列表~~ ✅ 2026-09-30 | **端到端可用的最简图床**（前端拖拽/粘贴上传、ASCII 进度队列、图库表格、详情栏四种分享格式） |
| M4 | ~~文件夹树 + 图片移动 + 上传到指定目录~~ ✅ 2026-09-30 | **树状存储成型**（侧栏目录树、可点面包屑、行内新建、非空不可删、图片跨目录移动）。目录**改名**未做——它需要批量迁移 R2 对象，归入 M5 |
| M5 | 目录重命名/移动的异步迁移任务 + 进度展示 | 树可自由重组 |
| M6 | 批量操作 + 回收站 + 配额展示 | 功能完整 |
| M7 | 部署上线 + 备份 | 可用 |

M3 结束时就已经有一个能用的图床，之后每个阶段都是增量。

### 10.1 ⚠️ 私有图片：4.5 节的原设计不成立，已搁置

4.5 节写的「私有对象单独放 `private/` 前缀且**不绑定域名**」**做不到**——
R2 的自定义域名绑定的是**整个 bucket**，绑不到前缀上（S3 也没有「只公开某个前缀」的能力）。
实测（2026-09-30，探针对象事后已删除）：

```
https://img.june-t.top/__probe.txt          → 200  对照
https://img.june-t.top/private/__probe.txt  → 200  ← 私有前缀被公开
https://img.june-t.top/nope-xyz.txt         → 404  基线，证明不是通配
```

也就是说，照 4.5 节实现出来的私有图片**一点保护都没有**，只是「别人猜不到」。
在做成之前不要按那一节动手。

要真正隔离只有两条路：

| | A. Cloudflare 防火墙规则 | B. 独立 bucket |
|---|---|---|
| 隔离强度 | 依赖路径匹配 | 物理隔离，无绕过 |
| 已知坑 | `/cdn-cgi/image/<opts>/private/x.jpg` 走转换路径，规则必须覆盖；路径编码/归一化也是 WAF 的经典绕过点 | 无 |
| 配置位置 | 活在控制台里，不在版本控制内 | 在 `config.yaml` 里 |

两条都需要额外的 Cloudflare 配置权限，**当前决定搁置**。

## 11. 明确不做

- **不做 OAuth2 / OIDC**：Ucenter 不支持，接入方式固定为「密码换 JWT + JWKS 本地验签」。
- **不做团队协作 / 共享空间 / 权限角色**：所有数据按 `owner_id` 硬隔离，无分享给其他用户的能力。
- **不做图片编辑**：裁剪、旋转、滤镜、加水印一律不做。
- **不做浏览器直传 R2**：上传统一走后端，换取鉴权、去重和元数据一致性。因此也不需要配置 R2 的 CORS。
- **不做自建缩略图服务**：列表页用原图 + `loading="lazy"` + CSS 缩放。若后续确实卡顿，接入 Cloudflare Image Resizing（付费）而不是自己写图像处理。
- **不做相对路径的 `/cdn-cgi/image/` 变换**：除非确认目标域名已开通 Image Resizing，否则第 7.4 节的「CDN 变换」格式先降级为直链。
- **不做移动端 App**：响应式适配桌面优先，窄屏隐藏 inspector。

## 12. 已确认的决策

| # | 决策 | 结论 |
|---|---|---|
| 1 | 存储 | 新建 R2 bucket `image`，`prefix` 留空（不与 `db-backup` / `music` 共用） |
| 2 | 图片直出域名 | `https://img.june-t.top`（绑定到 R2 bucket 的自定义域名） |
| 3 | 前端站点域名 | `https://img.taojun.top`（Cloudflare Pages） |
| 4 | 缩略图 | 不做 |
| 5 | 回收站 | 做，保留 **7 天** |
| 6 | 前端技术栈 | Vue 3 + Vite + TypeScript |
| 7 | Ucenter 应用标识 | `imgbed` |
| 8 | 元数据存储 | OCI HeatWave（MySQL 兼容），不用 SQLite |
| 9 | R2 访问路径 | 从 OCI 的 Go 服务走 S3 兼容 API，不额外引入 Worker（理由见 4.6） |
| 10 | 后端 API 域名 | `https://imgbed.june-t.top` |
| 11 | 数据库实例 | 复用现有 `mysql20260916150303`（Always Free 档），建独立库 `imgbed`（见第 13 节） |

### 12.1 后端 API 的对外域名

已定为 **`https://imgbed.june-t.top`**，沿用工作区既有约定
（`{app}.taojun.top` 放前端、`{app}.june-t.top` 放后端 API，见 `Remind-web/wrangler.jsonc`）。

前端通过 `VITE_API_BASE` 注入该地址，不写死在代码里；后端自身不感知对外域名。

## 13. 数据库实例（已就绪）

本机实测确认，**直接用现成实例，不需要新建**：

| 项 | 值 |
|---|---|
| 实例 | `mysql20260916150303`（OCID `ocid1.mysqldbsystem.oc1.ap-singapore-2.aaaaaaaayohpt2p6a3uc54k3qmqk524jc6rgnwgxvew4hixlhq6pnjdkg7dq`） |
| 规格 | **`MySQL.Free`** —— OCI Always Free 档，**不计费** |
| 版本 | MySQL 26.7.0-cloud |
| 端点 | `10.0.0.226:3306`，**仅私网**（`mysql.publicsubnet.myfreevcn.oraclevcn.com`） |
| 字符集 | `utf8mb4` / `utf8mb4_0900_ai_ci`，与本项目 schema 默认值一致 |
| `lower_case_table_names` | `0`（表名大小写敏感） |
| `require_secure_transport` | `0`（不强制 TLS，但服务端支持，实际连接已加密为 TLS_AES_128_GCM_SHA256） |
| 业务库 | **`imgbed`**（本项目专用，已创建） |
| 账号 | `junet@%`（`ON *.*` 全权，与 `/home/ubuntu/db/.env` 中的 `SRC_MYSQL_USER` 同一个） |

### 13.1 关于「HeatWave」这个名字

实例的 `heatwave-cluster` **未挂载**，也就是说它是 **MySQL 本体，没有启用 HeatWave 加速引擎**。
对本项目没有任何影响——我们只做 OLTP 点查，用不上列式内存分析。

### 13.2 网络前提

api 服务**必须跑在同一个 VCN 内**（即本机 `instance-20260820-1632`，私网 `10.0.0.180`），
因为 MySQL 只有私网端点。跨机部署需另配公网端点或跳板机。

### 13.3 备份覆盖（⚠️ 待处理）

现有备份脚本 `/home/ubuntu/db/bin/backup.sh` 用
`mysqldump --databases "$DB_NAME"` 备份，而 `DB_NAME` 取自 `SRC_MYSQL_DATABASE=appdb`：

```
bin/backup.sh:15   DB_NAME="${SRC_MYSQL_DATABASE:?SRC_MYSQL_DATABASE 未设置}"
bin/backup.sh:60   --databases "$DB_NAME" | gzip -9 > "$WORK/appdb.sql.gz"
```

**因此新库 `imgbed` 目前不在备份覆盖范围内。** 两个选项：

| 选项 | 代价 |
|---|---|
| A. 把 imgbed 的表放进 `appdb` | 零改动，但两个项目的数据混在一个库里 |
| B. 保留独立库，改 `backup.sh` 支持多库 | 需改两行（`--databases $DB_NAME` 去引号 + dump 文件名泛化），并同步更新 RUNBOOK |

**建议 B。** 改动很小，且保住「数据库隔离」与「备份覆盖」两件事。
