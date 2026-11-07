# nas 分支：自用补丁 + 本机构建 + 部署到群晖

本分支 = 上游 release tag + 少量自用修复提交。镜像在这台 PC 上用 Docker Desktop
构建，通过 `docker save | docker load` 推到 NAS，不经过任何镜像仓库。

## 仓库约定

| remote     | 地址                                     | 说明                       |
|------------|------------------------------------------|----------------------------|
| `origin`   | https://github.com/caojunCC/VoCat        | 自己的 fork，推 `nas` 分支 |
| `upstream` | https://github.com/MengMengCode/VoCat    | 官方仓库，只拉不推         |

- `master` 跟随 `upstream/master`，不放自己的提交。
- `nas` = 当前使用的上游 tag + 自用提交；只合并上游 **release tag**，不合并 master。
- 本仓库已设 `core.autocrlf=false`，避免 Windows 检出成 CRLF 导致 gofmt / shell 脚本出问题。

## 当前自用补丁

1. `fix(ims)`：Vodafone UK 在 183 里给出 SDP answer，200 OK 不带 body，
   官方版本会在接通瞬间把呼叫判失败并发 BYE。（`internal/vowifi/ims/call_runtime.go`）
2. `fix(ussi)`：USSI INVITE 的 SDP `a=rtpmap` 缺少时钟频率，Vodafone 返回 488；
   同时把 SIP 拒绝原因 / Warning 带进错误信息。（`internal/vowifi/ims/ussi_runtime.go`）

上游修好后，合并时这些提交会冲突或变成空提交，直接采用上游版本即可。

## 升级到上游新版本

```bash
git fetch upstream --tags
git checkout master && git merge --ff-only upstream/master && git push origin master
git checkout nas
git merge v0.3.43            # 换成新的 tag；有冲突就解决
```

## 测试（Docker 里跑，不需要本机装 Go）

```bash
MSYS_NO_PATHCONV=1 docker run --rm -v "$PWD:/src" -w /src \
  -e GOPROXY=https://goproxy.cn,direct -e GOFLAGS=-buildvcs=false \
  docker.m.daocloud.io/library/golang:1.25-alpine \
  sh -c 'go vet ./... && go test ./internal/...'
```

Go 版本以 `go.mod` / `Dockerfile` 为准。`internal/server` 里有个测试会因为容器里存在
`/.dockerenv` 失败，属于环境问题（`rm /.dockerenv` 后通过）。

## 构建

```bash
python nas/build.py               # 生成 vocat:<版本>-nas.<提交数>.<sha> 和 vocat:nas-latest
python nas/build.py --allow-dirty # 带未提交改动构建（tag 末尾加 .dirty）
```

- 上游 `Dockerfile` 不改；脚本只在临时副本里加 Docker Hub 镜像源
  （`REGISTRY_MIRROR`，默认 `docker.m.daocloud.io`）和 `GOPROXY`（默认 goproxy.cn）。
- 程序内版本号是 `0.3.42+nas.N.sha`，`+` 后的元数据不影响 vocat 的检查更新逻辑。
- 构建出的镜像名写在 `nas/.last-build`（已忽略，不入库）。

## 部署到 NAS

需要 `pip install paramiko`。密码从环境变量 `NAS_PASSWORD` 读取，没设置就交互输入，不落盘。
主机 / 端口 / 用户可用 `NAS_HOST` / `NAS_PORT` / `NAS_USER` 覆盖（默认 192.168.2.245 / 8022 / caojun176）。

```bash
python nas/deploy.py              # 部署 nas/.last-build 里的镜像
python nas/deploy.py vocat:xxx    # 部署指定镜像
```

部署流程：传镜像 → NAS 上 `docker load` → 停掉 `vocat` → 把整个 vocat-data 卷备份到
`~/vocat-backups/<时间>-before-<镜像>/` → 旧容器改名 `vocat-prev` 保留 → 用同样参数启动新
`vocat` → 等待 IMS 注册（最多 3 分钟）并输出状态。

## 回滚

```bash
python nas/deploy.py --rollback               # 切回 vocat-prev（数据库保持现状）
python nas/deploy.py --rollback --restore-db  # 同时恢复最近一次部署前的数据库备份
```

新版本如果带了数据库迁移，旧版本可能不认新的 `user_version`，这时用 `--restore-db`
（会丢掉部署之后产生的短信 / 通话记录）。
