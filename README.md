# 轻连 · Clearline

macOS 上连接自有服务器的客户端。每位用户授权自己的 DigitalOcean 账号，优先选择并复用已有服务器和套餐，也可导入已有连接。提供 **AI 分流 / 全部代理**、连接阶段、出口 IP 验证和网络测速。

**当前为本地构建 7，尚未发布新的 GitHub Release。** 界面只提供 **AI 分流 / 全部代理**；两种模式均接管系统 DNS 和公网 TCP/UDP，再按域名决定解析与连接去向。原“全局隧道”设置迁移为“全部代理”。本轮分流功能已通过自动化测试，实机验收正在进行；先前全局接管已在 Apple Silicon Hysteria 2 连接中验证，不能据此替代新分流验收。

开发此分支请运行 `./scripts/build.sh`，退出旧客户端后通过完整安装包一次更新客户端和匹配网络组件。服务器始终由各用户拥有和付费。

## 安装与首次使用

1. 通过完整安装包安装 Clearline，或将本机构建的 `Clearline.app` 放入“应用程序”。需要 macOS 13+；构建包含 Apple Silicon 和 Intel 版本。
2. 点击“设置连接”，默认使用“已有服务器”。选择 Ego Lite 或系统默认浏览器，在官方授权页确认 **Clearline for macOS** 的权限。应用显示授权进度与有效期；仅登录官网控制台不等于完成应用授权。
3. 选择自己的已有服务器，查看原套餐和区域。点击“读取并保存已有连接”。应用先尝试本机已信任的 SSH；没有可用访问权限时，使用“复制命令并打开控制台”，运行后复制加密连接码，返回应用粘贴保存（也支持 ⌘V）。整个读取过程不新建或升配服务器。
4. 点击“完成并连接”。应用先检查其他代理和 VPN 对服务器入口的影响；处理提示后再连接，首次接入会自动验证出口。连接与凭据保存在本机钥匙串。

已有 Hysteria 2 链接或 WireGuard / AmneziaWG 配置时，可选“导入配置”；加密 `.clearline` 文件也可从文件菜单导入。公开下载不提供个人服务器连接、密码或令牌。

已有服务器读取支持常见 Hysteria 2 JSON/YAML 配置、password/userpass 认证和现有 TLS 证书。控制台读取需要服务器上的 Python 3 与 cryptography；缺少组件或使用其他认证时会给出导入配置的替代入口，不自动安装组件、重置密码、添加 SSH 密钥或修改服务。WireGuard 客户端需要独立身份，使用自己的客户端配置导入。

“新建服务器”是独立选项，需要另外授权创建权限、查看当前报价并在应用中明确确认持续费用；它会新增一台独立 Droplet。账号检查或已有服务器接入不授权创建收费资源。

DigitalOcean 独立计费，应用展示创建前读取的价格；税费、超额流量及最终账单以 [DigitalOcean 账单规则](https://docs.digitalocean.com/products/billing/) 为准。关闭应用或停止连接后，服务器会继续存在。需要停止服务器费用时，请在 DigitalOcean 控制台删除对应 Droplet。应用不自动删除云资源。

## 让 AI 协助

在设置界面点击“让 AI 协助”，把复制的说明交给 Codex 或 Claude Code。也可把以下内容直接交给本机 AI：

> 按照这个仓库的 AGENTS.md，帮我用自己的 DigitalOcean 账号接入 Clearline，优先沿用已有服务器和套餐。登录、OAuth 和系统安全提示由我本人完成。通过应用读取或配对已有连接，先检查其他代理的影响，再验证出口。只有我另行选择新建并确认费用，才创建收费资源。

AI 可以读取 `~/Library/Application Support/Clearline/setup-status.json`。Schema 2 分开记录实际客户端的 `connection` 和账号设置的 `cloud`；顶层 `connectionSaved`、`connected` 反映本机真实连接，不会被旧的新建方案覆盖。未读取账号时授权状态可为空；不要据此重新要求已有连接授权。文件不包含账号 ID、服务器地址、连接内容或令牌，不需要额外 AI API Key。

## 连接范围

- **AI 分流**：Claude、ChatGPT / OpenAI / Codex 及必要登录、附件、静态资源域名，DNS 经加密连接从服务器解析，TCP/UDP 也经同一服务器。其他域名使用连接前的本地 DNS，并通过物理网卡直连。清单见 [ai-domains.txt](core/ai-domains.txt)。
- **全部代理**：公网 IPv4/IPv6 的 TCP、UDP 与系统 DNS 均通过服务器。原“全局隧道”设置对应此模式，不再额外显示第三个选项。

AI 分流中，已由本地 DNS 解析的普通域名 UDP 保持直连；无法识别域名的 UDP 和常见 STUN 端口经服务器保护出口，避免 WebRTC 暴露本地公网地址。局域网、回环和服务器入口保留直连，ICMP 不转发。Hysteria 2 服务端需启用 UDP，服务器入口当前要求可用的 IPv4 地址。

分流 DNS 给 AI 域名分配内部地址，保留域名到后续 TCP/UDP 的关联，避免共享 CDN 地址导致普通网站被误分流。地址映射仅保存到本机 `~/Library/Application Support/Clearline/dns-map-v1.json`（0600），以免重连后浏览器缓存指向错误域名；不含密码、令牌或浏览 URL，不上传。未知的旧内部地址会拒绝连接。AI DNS 不向本地回退，本地 DNS 失败也不会自动改到服务器。域名按边界匹配，`chatgpt.com.evil.example` 不会命中；共享登录或验证域名按同一清单处理。

完整安装包同时更新客户端和网络组件。ZIP 安装方式可从应用打开配套 `ClearlineSystemTunnel.pkg`，由电脑主人完成 macOS 管理员认证。组件不接收连接密码，只管理虚拟网卡、DNS 和必要路由；它只接受当前构建中封装的精确内核签名。无需新增或升级服务器。

只有实际 DNS 数据路径、服务器 TCP/UDP 出口与有效系统 DNS 接管验证通过后，才显示保护状态。接管失败时先恢复网络，再启用临时系统代理并明确显示“DNS / UDP 未保护”；这只是故障恢复，不是第三种可选模式。再次连接会重试正常隧道。系统代理被其他软件覆盖时另行提示，不把服务器通道连通当作系统已接管。

“工具 → 检查 DNS / UDP 隧道”可在临时网卡上测试 IPv4/IPv6 的 UDP/TCP DNS 和服务器 UDP 出口，不修改系统 DNS、默认路由或现有代理。检测通过本身不代表系统保护已启用。Hysteria 2 可保持临时系统代理连接同时诊断；WireGuard 类连接需先断开，避免同一身份并行建连。

连接状态显示阶段与耗时，唤醒或网络变化会检查连接。传输暂时中断时保留隧道接管并重试；主动断开、退出或进程结束时恢复本组件管理的网络设置，保留后来由其他软件修改的配置。

## 出口与测速

“验证出口 IP”通过当前加密隧道请求 [Cloudflare trace](https://developers.cloudflare.com/fundamentals/reference/cdn-cgi-endpoint/)，显示实测公网 IP、地区和验证时间。检测失败时显示错误。AI 分流下，普通 IP/DNS 检测网站按普通域名规则直连和本地解析，可能仍显示国内出口；内置验证单独核对服务器出口。

“网络测速”包含匿名 Claude HEAD 响应、普通网络响应和最多 5 MB 下载采样。403 / 429 会显示访问受限提示；这些结果不证明 Claude 账号会话可用。普通响应分别显示首次访问和复用连接的数据，下载速率包含建连耗时。测速遵循当前代理范围，可随时取消。程序没有遥测和上传测速结果的功能。

## 授权、费用保护与隐私

DigitalOcean 登录使用 [官方公共客户端 OAuth + PKCE](https://docs.digitalocean.com/reference/api/oauth/)，默认只申请账号和服务器读取权限；选择新建才申请创建权限。重试前同步登记的回调地址，避免旧端口导致授权失效。应用不接收账号密码或共用 OAuth secret；令牌和连接保存在本机钥匙串，通过私有管道传递。

已有连接配对使用一次性 RSA-OAEP / AES-GCM，加密内容绑定所选服务器和本次会话，15 分钟过期、成功后不可重放。应用核对服务器地址、证书身份和有效期；服务器证书私钥不会被读取或传输。自动 SSH 不绕过主机密钥校验、不请求密码，也不建立新的服务器访问权限。

创建操作必须对应用户刚确认的方案；应用创建前再次核对价格和可用性。创建请求前，恢复信息需成功保存到钥匙串。请求结果不确定时，应用按已有部署记录核对服务器，停止自动重复创建。关闭设置窗口会停止本机等待；已发出的创建请求和已存在的 Droplet 仍需在 DigitalOcean 管理。

服务器部署固定版本、校验 SHA-256 的 Hysteria 2。每次部署生成独立密码与 TLS 证书，客户端验证该证书和主机名。服务端限制访问本机、私有网络和云元数据地址。服务器初始化数据包含该用户自己的服务器凭据，交付给其 DigitalOcean 账号下的 Droplet；请保护自己的云账号。

加密迁移文件使用 Argon2id 和 XChaCha20-Poly1305。配置密码单独传递。多台电脑同时使用 WireGuard 时需要各自独立身份。公开源码、安装包和发布记录不包含开发者个人服务器地址或连接凭据。GitHub 仓库所属账号仍由 GitHub 正常显示。

临时系统代理的恢复记录位于 `~/Library/Application Support/Clearline/network-restore.plist`，权限为 `0600`。正常隧道连接不设置系统代理。网络组件安装在 `/Library/PrivilegedHelperTools/org.clearline.tunnel`，由 launchd 按需唤起；无连接时空闲 30 秒退出，启动时会检查遗留恢复记录。root 专用记录持久保存在 `/Library/Application Support/Clearline/system-tunnel-state.plist`（`0600`），不会因重启清空。组件在断开、内核退出、自身重启或系统启动时恢复所管理的 DNS/路由，保留后来由其他软件修改的配置。传输暂时中断时保留隧道接管，DNS 不回退到本地。电脑主人可用 `sudo ./scripts/uninstall-tunnel.sh` 卸载组件；脚本在网络未恢复时停止卸载。更新前先准备备用联网方式。

## 构建与验证

需要 Xcode / Swift 工具链与网络连接：

```sh
./scripts/build.sh
swift test
cd core
go test -race ./...
```

构建产物位于 `dist/`，包括一次安装客户端和组件的完整安装包、应用 ZIP，以及应用内的配套组件安装包。构建脚本固定 Go 工具链与校验值，可用 `CLEARLINE_GO` 指定已有工具链；Go 依赖由 `go.mod` / `go.sum` 固定。组件的内核签名白名单封装在自身只读代码段，不从用户可修改的配置读取。构建过程映射本机源码路径，发行包需再执行隐私扫描。

仅更新网络组件时，可运行 `./scripts/build-tunnel-component.sh /Applications/Clearline.app/Contents/Helpers/clearline-core`，生成 `dist/ClearlineSystemTunnel-hotfix.pkg`。它将所选已安装内核的精确签名哈希封装到新组件中，不替换客户端和内核；安装前先断开当前隧道，准备备用联网方式。该路径已用于构建 5 客户端搭配构建 6 组件的实机修复，避免为组件更新重复请求钥匙串读取。

发行包采用本地签名，尚未使用 Developer ID 公证。目标 Mac 的 Gatekeeper 可能要求电脑主人确认打开，也可在目标 Mac 从源码构建。Intel 版本完成交叉编译，尚未完成实体设备验收。

测试使用本地假 API、授权回调和服务器配置，覆盖只读授权、已有小规格套餐、分页、费用保护、加密配对的会话/地址/证书绑定、过期与重放、钥匙串保存失败、代理冲突预检、真实连接状态与新建向导隔离，以及原有部署恢复与协议验证。Python 读取器集成测试可用 `CLEARLINE_PAIRING_PYTHON` 指定含 cryptography 的解释器。测试不创建收费资源、不使用个人账号或 AI 登录会话；本地测试不等于真实账号端到端验收。

## 许可

MIT。传输依赖 [Hysteria 2](https://github.com/HyNetworks/hysteria) 和 [AmneziaWG-Go](https://github.com/amnezia-vpn/amneziawg-go)。完整依赖许可随应用保存在 `Contents/Resources/ThirdPartyNotices.txt`，分享应用时应保留。
