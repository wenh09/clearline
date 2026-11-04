# 轻连 · Clearline

macOS 上的自助代理客户端。每位用户授权自己的 DigitalOcean 账号，为自己的账号部署美国服务器，应用自动生成并保存连接配置。日常一键连接，主界面保留 **AI 分流 / 全部代理**、连接阶段、出口 IP 验证和网络测速。

**0.3.0 是自助部署公开测试版。** 本地测试覆盖 OAuth、费用确认、部署请求、中断恢复和连接协议；真实新账号创建付费 Droplet 的完整验收仍待用户完成。仓库提供客户端和部署代码，服务器由各用户拥有和付费。

## 安装与首次使用

1. 从 [Releases](https://github.com/wenh09/clearline/releases/latest) 下载 `Clearline-macOS-universal.zip`，解压并把 `Clearline.app` 放入“应用程序”。需要 macOS 13+；包含 Apple Silicon 和 Intel 版本。
2. 打开应用，点击“设置 DigitalOcean”。在 DigitalOcean 官方网站登录自己的账号并授权，完成后返回应用。账号需已激活并设置付款方式。
3. 应用读取当前可用的美国基础服务器方案，展示官网标示的月价、小时价。确认由自己的 DigitalOcean 账号承担费用后，点击“确认费用并部署”。
4. 等待创建服务器、安装 Hysteria 2 和验证美国出口。连接保存到本机钥匙串后，点击“完成”，回到主界面连接。

已经有连接配置时，可从“文件 → 导入配置”导入自己的加密 `.clearline` 文件。公开下载不提供个人服务器连接文件、密码、令牌或公共代理服务。

部署会创建一台独立 Droplet。现有服务器和第三方 VPN 不参与新部署。已有个人服务器可继续通过自己的配置使用；自动接管、修改任意现有服务器的功能尚未提供。

DigitalOcean 独立计费，应用展示创建前读取的价格；税费、超额流量及最终账单以 [DigitalOcean 账单规则](https://docs.digitalocean.com/products/billing/) 为准。关闭应用或停止连接后，服务器会继续存在。需要停止服务器费用时，请在 DigitalOcean 控制台删除对应 Droplet。应用不自动删除云资源。

## 让 AI 协助

在设置界面点击“让 AI 协助”，把复制的说明交给 Codex 或 Claude Code。也可把以下内容直接交给本机 AI：

> 按照这个仓库的 AGENTS.md，帮我安装 Clearline 并使用我自己的 DigitalOcean 账号完成首次设置。登录和授权由我本人完成。创建服务器前展示应用读取的价格，等我在应用中确认费用。通过应用部署，读取本机的脱敏状态了解进度，完成后核对出口 IP。

AI 可以检查代码、协助安装和读取 `~/Library/Application Support/Clearline/setup-status.json`。该状态文件只包含阶段、是否需要处理、是否保存连接、区域和价格等信息。AI 无需读取 DigitalOcean 令牌、服务器私钥或连接密码。应用使用固定部署流程，不需要配置额外的 AI API Key。

## 连接范围

- **AI 分流**：Claude、ChatGPT / OpenAI / Codex 及必要的登录、附件、静态资源域名走服务器；其他域名直连。清单见 [ai-domains.txt](core/ai-domains.txt)。
- **全部代理**：所有进入 macOS HTTP / HTTPS / SOCKS 系统代理的 TCP 请求走服务器。

域名按边界匹配，`chatgpt.com.evil.example` 不会命中 ChatGPT 规则。共享登录、验证主机按域名统一代理。隧道失败时，代理域名不会自动回退到本机直连。旧“仅 Claude”设置迁移为“AI 分流”。

这是系统代理模式。不遵循系统代理的应用和任意应用的 UDP 流量需要其它接入方式。终端工具可从“工具”复制代理命令；临时端口在重新连接后可能变化。

连接状态包含服务器连接、隧道验证、接入 Mac 的阶段与耗时。传输中断时会保留本地代理接入并尝试恢复，Mac 唤醒或网络变化后也会检查连接。主动断开、退出或整个本地代理进程结束时，恢复先前的系统代理；后续由其它软件修改的代理设置会保留。

## 出口与测速

“验证出口 IP”通过当前加密隧道请求 [Cloudflare trace](https://developers.cloudflare.com/fundamentals/reference/cdn-cgi-endpoint/)，显示实测公网 IP、地区和验证时间。检测失败时显示错误。AI 分流下，浏览器访问普通 IP 检测网站会按规则直连；内置验证单独核对隧道出口。

“网络测速”包含匿名 Claude HEAD 响应、普通网络响应和最多 5 MB 下载采样。403 / 429 会显示访问受限提示；这些结果不证明 Claude 账号会话可用。普通响应分别显示首次访问和复用连接的数据，下载速率包含建连耗时。测速遵循当前代理范围，可随时取消。程序没有遥测和上传测速结果的功能。

## 授权、费用保护与隐私

DigitalOcean 登录使用 [官方公共客户端 OAuth + PKCE](https://docs.digitalocean.com/reference/api/oauth/)，由本机回调完成授权。应用不接收 DigitalOcean 登录密码，不内置共用 OAuth secret。授权令牌、刷新令牌、部署凭据和服务器连接保存在本机登录钥匙串，凭据通过进程私有管道传递。

创建操作必须对应用户刚确认的方案；应用创建前再次核对价格和可用性。创建请求前，恢复信息需成功保存到钥匙串。请求结果不确定时，应用按已有部署记录核对服务器，停止自动重复创建。关闭设置窗口会停止本机等待；已发出的创建请求和已存在的 Droplet 仍需在 DigitalOcean 管理。

服务器部署固定版本、校验 SHA-256 的 Hysteria 2。每次部署生成独立密码与 TLS 证书，客户端验证该证书和主机名。服务端限制访问本机、私有网络和云元数据地址。服务器初始化数据包含该用户自己的服务器凭据，交付给其 DigitalOcean 账号下的 Droplet；请保护自己的云账号。

加密迁移文件使用 Argon2id 和 XChaCha20-Poly1305。配置密码单独传递。多台电脑同时使用 WireGuard 时需要各自独立身份。公开源码、安装包和发布记录不包含开发者个人服务器地址或连接凭据。GitHub 仓库所属账号仍由 GitHub 正常显示。

系统代理的恢复记录位于 `~/Library/Application Support/Clearline/network-restore.plist`，权限为 `0600`。程序不安装常驻 root 服务，不修改 Mac 的 DNS 和系统路由。更新运行中的客户端前，请先验证备用联网连接；新版接管后再撤掉备用连接。

## 构建与验证

需要 Xcode / Swift 工具链与网络连接：

```sh
./scripts/build.sh
swift test
cd core
go test -race ./...
```

构建产物位于 `dist/`。构建脚本固定 Go 工具链与校验值，可用 `CLEARLINE_GO` 指定已有工具链；Go 依赖由 `go.mod` / `go.sum` 固定。构建过程映射本机源码路径，发行包需再执行隐私扫描。

发行包采用本地签名，尚未使用 Developer ID 公证。目标 Mac 的 Gatekeeper 可能要求电脑主人确认打开，也可在目标 Mac 从源码构建。Intel 版本完成交叉编译，尚未完成实体设备验收。

云部署测试使用本地假 API 和授权回调，覆盖 PKCE、防伪回调、过期方案、价格变化、钥匙串保存失败、创建结果不确定和已有部署恢复。协议测试覆盖真实回环 Hysteria 2 / WireGuard 服务、HTTP / SOCKS、分流和禁止直连回退。测试不创建收费资源，不使用个人账号或 Claude 登录会话。

## 许可

MIT。传输依赖 [Hysteria 2](https://github.com/HyNetworks/hysteria) 和 [AmneziaWG-Go](https://github.com/amnezia-vpn/amneziawg-go)。完整依赖许可随应用保存在 `Contents/Resources/ThirdPartyNotices.txt`，分享应用时应保留。
