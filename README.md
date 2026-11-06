<div align="center">
  <h1>Imgbed · Backend</h1>
  <p>自托管图床后端 · Go 单二进制，Cloudflare R2 对象存储 + MySQL 元数据</p>
</div>

<div align="center">

![Go](https://img.shields.io/badge/Go-1.26%2B-00ADD8)
![Gin](https://img.shields.io/badge/Gin-v1-008ECF)
![R2](https://img.shields.io/badge/Storage-Cloudflare%20R2-F38020)
![License](https://img.shields.io/badge/License-MIT-green)

</div>

## 项目定位

一个自托管图床的**后端 API**。前端是独立的 Vue 3 应用（[Imgbed-web](https://github.com/CheeseTJ/Imgbed-web)），
本仓库只提供接口与对象读写。存储走 Cloudflare R2（S3 兼容），元数据走 MySQL
（OCI HeatWave 兼容），二进制本身无状态，可以随时重开。

```text
浏览器 ──> Imgbed-web (Vue 3 + Vite)
                │  fetch /api/*  (JWT)
                ▼
          本仓库 (Go + Gin)
                ├──> Cloudflare R2   图片对象（S3 兼容 API）
                └──> MySQL           元数据：图片 / 文件夹 / 回收站 / 配额
```

## 功能

- **上传** —— 单张 / 批量（默认上限 50）、拖拽直传、20 MiB 单文件限制
- **图库** —— 分页列表、按文件夹筛选、搜索、批量操作
- **文件夹** —— 树形结构、重命名、批量迁移（超过阈值时给出警告）
- **回收站** —— 软删除 + 保留期（默认 7 天）自动清理
- **存储占用** —— R2 真实用量（调 Cloudflare API 读），失败时回退到数据库口径
- **私有对象** —— `private/` 前缀下的对象不绑定自定义域名，只走签名访问
- **鉴权** —— 统一身份中心签发的 JWT，用 JWKS 校验 `aud`；不自己管密码
- **健康检查** —— `/api/health` 同时探活数据库与 R2

## 快速开始

### 1. 准备配置

```bash
cp config.yaml.example config.yaml
```

配置分三块：`server`（监听地址、CORS 白名单）、`r2`（R2 凭据与桶）、`db`（MySQL
连接）。**密钥建议全部走环境变量**，`config.yaml` 里留空即可，见
`internal/config/config.go` 的 `applyEnv`。

### 2. 环境变量

```bash
# R2
export IMGBED_R2_ACCOUNT_ID=...
export IMGBED_R2_ACCESS_KEY_ID=...
export IMGBED_R2_SECRET_ACCESS_KEY=...
export IMGBED_R2_CF_API_TOKEN=...   # 查真实占用用，可选

# 数据库
export IMGBED_DB_PASSWORD=...
```

完整清单见 `internal/config/config.go`。

### 3. Docker 启动（推荐）

```bash
docker compose up -d --build
curl http://localhost:18080/api/health
```

也可直接跑二进制：

```bash
go build -o bin/imgbed ./cmd/imgbed
./bin/imgbed
```

## 配置说明

| 段 | 关键项 | 说明 |
|---|---|---|
| `server` | `addr` | 监听地址，默认 `:18080` |
| `server` | `allowed_origins` | CORS 白名单，**留空 = 全放行** |
| `r2` | `bucket` / `prefix` | 桶名与前缀，前缀留空 = 桶根目录 |
| `r2` | `public_base` | 公开读的自定义域名 |
| `r2` | `private_prefix` | 私有对象前缀，默认 `private/` |
| `r2` | `quota_bytes` | 配额分母，默认 10 GiB（R2 免费档） |
| `db` | `dsn` / 分字段 | 两种给法二选一，推荐分字段 + 环境变量密码 |
| `db` | `tls_mode` | 对齐 go-sql-driver；HeatWave 自签证书用 `preferred` |
| `ucenter` | `base_url` / `app` | JWT 校验来源，`app` 须与身份中心的 APPS 一致 |

## 目录结构

```text
cmd/imgbed/            入口
internal/api/          HTTP 路由与 handler
internal/auth/         JWT / JWKS 校验
internal/r2/           R2（S3 兼容）客户端
internal/store/        MySQL 访问层
internal/imageinfo/    图片尺寸/格式探测
internal/idgen/        ID 生成
internal/config/       配置加载 + 环境变量覆盖
SPEC.md                接口与数据模型规格
```

## 部署

仓库自带 `.github/workflows/deploy.yml`：push 到 `main` 后跑 `go test ./...`，再由
self-hosted runner 写配置、构建镜像并 `docker compose up -d --build`。

> 该 job 含 `if: github.repository_owner == 'CheeseTJ'` 门禁——self-hosted runner 与
> 生产服务同机，这道门禁防止外部 fork/PR 触发在服务器上执行代码。

## License

[MIT](./LICENSE) © 2026 JuneT
