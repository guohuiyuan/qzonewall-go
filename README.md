# QzoneWall-Go

一个给 QQ 群用的「表白墙/投稿墙」服务。

它把投稿收集、审核、渲染截图、发布到 QQ 空间这几件事串在一起：
- 群内可以投稿、匿名投稿、撤稿
- 管理员可以看稿、过稿、拒稿、批量处理
- Web 后台可以审核、扫码登录、看发布状态
- Worker 会按频率限制自动发布到 QQ 空间

项目基于 Go，数据存 SQLite，机器人侧使用 ZeroBot（对接 NapCat WebSocket），QQ 空间接口由 `qzone-go` 提供。

## 主要能力

- 投稿来源
  - QQ 群命令投稿
  - Web 投稿页投稿
- 审核流程
  - `pending -> approved -> published`
  - 失败会落到 `failed`，并记录失败原因
- 发布方式
  - 发布前将投稿渲染成一张截图（文字+图片）
  - 再把截图作为图片发到 QQ 空间
- Cookie 管理
  - 启动后异步尝试 `GetCookies`（优先）
  - 失败再回退到扫码登录
  - 会话过期时自动触发刷新回调
- 安全与数据
  - SQLite 持久化（WAL）
  - Web 管理后台账号+会话
  - 可配置敏感词过滤

## 项目结构

```text
qzonewall-go/
├─ cmd/wall/main.go                # 程序入口
├─ internal/config/                # 配置加载与默认值
├─ internal/source/qq_bot.go       # QQ Bot 命令与事件处理
├─ internal/task/worker.go         # 审核后自动发布 Worker
├─ internal/task/keepalive.go      # Cookie 校验/刷新/扫码逻辑
├─ internal/web/server.go          # Web 后台与投稿页
├─ internal/render/screenshot.go   # 投稿截图渲染
├─ internal/store/sqlite.go        # SQLite 存储
├─ config.yaml                     # 配置文件
├─ run.bat / run.sh                # 启动脚本
├─ Dockerfile                      # Docker 构建文件
├─ .dockerignore                   # Docker 忽略文件
└─ winres/                         # Windows 资源文件
```

## 环境要求

- Go `1.24+`
- 一个可用的 NapCat + ZeroBot WebSocket 连接

## 快速开始

1. 修改 `config.yaml`
2. 确认 NapCat WebSocket 可连
3. 启动程序

Windows:

```bat
run.bat
```

Linux/macOS:

```bash
chmod +x run.sh
./run.sh
```

或者直接：

```bash
go mod tidy
go generate ./...
go run ./cmd/wall
```

## Docker 部署

项目提供了 Docker 支持，可以快速部署到容器环境中。

### 快速部署（推荐）

使用自动部署脚本：

```bash
# 下载部署脚本
wget https://raw.githubusercontent.com/guohuiyuan/qzonewall-go/main/deploy.sh
chmod +x deploy.sh

# 运行部署脚本
./deploy.sh
```

脚本会自动：
- 拉取最新 Docker 镜像
- 创建示例配置文件
- 启动容器并挂载配置

### 手动部署

#### 构建镜像

```bash
docker build -t qzonewall-go .
```

#### 运行容器

基本运行（使用内置默认配置）：

```bash
docker run -p 8081:8081 qzonewall-go
```

自定义配置（推荐）：

```bash
# 复制并修改配置文件
cp cmd/wall/example_config.yaml my_config.yaml
# 编辑 my_config.yaml 进行自定义配置

# 运行容器并挂载配置
docker run -p 8081:8081 -v $(pwd)/my_config.yaml:/home/appuser/config.yaml qzonewall-go
```

### Docker 环境说明

- **端口**: 容器内部使用 8081 端口
- **配置**: 容器内包含默认配置，可以通过挂载覆盖
- **数据**: SQLite 数据库文件会自动在容器内创建
- **持久化**: 如需持久化数据，可以额外挂载数据库文件：

```bash
docker run -p 8081:8081 \
  -v $(pwd)/my_config.yaml:/home/appuser/config.yaml \
  -v $(pwd)/data.db:/home/appuser/data.db \
  qzonewall-go
```

## 配置说明（config.yaml）

### `qzone`

- `keep_alive`: Cookie 有效性轮询间隔
- `max_retry`: QQ 空间接口最大重试次数
- `timeout`: QQ 空间接口超时

### `bot`

- `zero.nickname`: 机器人昵称
- `zero.command_prefix`: 命令前缀（默认 `/`）
- `zero.super_users`: 超级用户 QQ
- `ws`: NapCat WebSocket 地址和 `access_token`
- `manage_group`: 管理通知群号

### `wall`

- `show_author`: 发布到空间时，非匿名稿件是否加署名
- `anon_default`: 投稿页默认是否勾选匿名
- `max_images`: 单条稿件最大图片数
- `max_text_len`: 单条稿件最大文本长度
- `publish_delay`: 额外发布延迟

### `database`

- `path`: SQLite 文件路径

### `web`

- `enable`: 是否启用 Web
- `addr`: 监听地址（例如 `:8081`）
- `admin_user` / `admin_pass`: 管理后台初始账号

### `censor`

- `enable`: 是否启用敏感词
- `words`: 内置敏感词列表
- `words_file`: 外部敏感词文件（每行一个）

### `worker`

- `workers`: Worker 数量
- `retry_count`: 发布失败重试次数
- `retry_delay`: 重试间隔
- `rate_limit`: 发布频率限制
- `poll_interval`: 拉取待发布稿件间隔

## QQ 命令

普通用户：

- `/投稿 <内容>`
- `/匿名投稿 <内容>`
- `/撤稿 <编号>`

管理员：

- `/看稿 <编号>`
- `/过稿 <编号>`（支持范围/批量，如 `1-4` 或 `1,2,5`）
- `/拒稿 <编号> [理由]`
- `/待审核`
- `/发说说 <内容>`
- `/扫码`
- `/刷新cookie`
- `/帮助`

## Web 页面与接口

页面路由：

- `/submit`: 投稿页
- `/login`: 管理登录页
- `/admin`: 管理后台

主要 API：

- `POST /api/submit`
- `POST /api/approve`
- `POST /api/reject`
- `POST /api/approve/batch`
- `POST /api/reject/batch`
- `GET /api/qrcode`
- `GET /api/qrcode/status`
- `GET /api/health`

静态资源：

- `/uploads/*`
- `/icon.png`
- `/favicon.ico`

## 启动时的 Cookie 流程（当前实现）

程序启动后会先把 Bot、Worker、Web 拉起来，然后后台异步执行 Cookie 引导流程：

1. 尝试从 Bot `GetCookies` 获取
2. 多次失败后回退到扫码登录
3. 成功后 `UpdateCookie`
4. 用 `GetUserInfo` 做一次有效性校验

所以你会看到系统先启动，再看到 cookie bootstrap 日志，这是预期行为。

## 数据库状态说明

`posts.status` 主要有 5 种：

- `pending`: 待审核
- `approved`: 已通过，待发布
- `rejected`: 已拒绝
- `failed`: 发布失败
- `published`: 已发布

## 开发与测试

```bash
go test ./...
```

Windows 发布资源（可选）：

```bash
go generate ./winres
```

## 常见问题

### 1) `GetCookies` 没有拿到值

先看 Bot 是否真的连上 NapCat；如果没有活跃 Bot 上下文，`GetCookies` 会拿不到。

### 2) 发布失败 `qzone api: code=-1`

这通常和 Cookie 失效、rkey 失效、图片源不可访问有关。建议先做三件事：

1. 重新走一次扫码登录
2. 检查图片链接是否可直连
3. 打开日志看 Worker 的重试结果

## 说明

这个项目在功能上是“能跑、能审、能发”的路线，代码也在持续迭代。如果你准备长期使用，建议先把 `admin_pass`、群权限、Web 暴露端口这些安全项收紧。
