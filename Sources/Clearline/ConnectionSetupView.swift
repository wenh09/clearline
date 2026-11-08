import AppKit
import SwiftUI

@MainActor struct ConnectionSetupView: View {
    @EnvironmentObject var app: AppModel
    @Environment(\.dismiss) private var dismiss
    @StateObject private var cloud: ConnectionSetupModel
    @State private var importText = ""
    @State private var saving = false
    private let accent = Color(red: 0.09, green: 0.55, blue: 0.47)
    init(model: ConnectionSetupModel? = nil) { _cloud = StateObject(wrappedValue: model ?? ConnectionSetupModel()) }

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            HStack {
                Image(systemName: "network").foregroundStyle(accent)
                Text("连接自己的服务器").font(.title2.weight(.semibold))
                Spacer()
                Button { dismiss() } label: { Image(systemName: "xmark").frame(width: 30, height: 30) }
                    .buttonStyle(InteractiveStyle(surface: .segment)).accessibilityLabel("关闭连接设置")
            }
            Picker("接入方式", selection: Binding(get: { cloud.intent }, set: cloud.selectIntent)) {
                ForEach(SetupIntent.allCases) { Text($0.title).tag($0) }
            }.pickerStyle(.segmented).disabled(cloud.working)
            ScrollView {
                VStack(alignment: .leading, spacing: 16) {
                    if !app.profiles.isEmpty {
                        Label("本机已有连接：" + (app.selected?.name ?? "已保存"), systemImage: "checkmark.shield")
                            .font(.callout).foregroundStyle(accent)
                    }
                    Text(cloud.message).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                    if let error = cloud.error { Text(error).font(.callout).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true) }
                    if cloud.working { HStack { ProgressView().controlSize(.small); Text(cloud.authorizationPending ? "等待应用授权完成…" : "处理中…").foregroundStyle(.secondary) } }
                    if cloud.saved {
                        Label("连接已保存到本机钥匙串", systemImage: "checkmark.circle.fill").foregroundStyle(accent)
                        action(app.connected ? "完成" : "完成并连接") {
                            if !app.connected { app.connectAfterSetup = true; app.verifyAfterSetup = true }
                            dismiss()
                        }
                    } else if cloud.profile != nil {
                        action("重新保存连接", enabled: !saving && !app.connected && !app.busy) { saveProfile() }
                    } else if cloud.intent == .importProfile {
                        importSection
                    } else {
                        HStack {
                            Label(cloud.authorized ? "账号已授权" : "1 · 授权账号", systemImage: cloud.authorized ? "checkmark.circle" : "person.crop.circle")
                            Spacer()
                            if cloud.authorized { Text("\(cloud.servers.count) 台已有服务器").font(.caption).foregroundStyle(.secondary) }
                        }.font(.callout.weight(.medium))
                        if !cloud.authorized || (cloud.intent == .create && !cloud.canCreatePermission) { accountSection }
                        else if cloud.intent == .existing { existingSection }
                        else { createSection }
                    }
                    if cloud.hasDeployment && cloud.intent == .existing && !cloud.saved {
                        Divider()
                        Text("账号中有上次由轻连创建的部署记录，可继续检查，避免重复创建。").font(.caption).foregroundStyle(.secondary)
                        Button("检查上次部署") { cloud.command("resume") }.disabled(cloud.working || app.connected || app.busy)
                    }
                }.frame(maxWidth: .infinity, alignment: .leading).padding(.vertical, 2)
            }
            Divider()
            HStack {
                Button("让 AI 协助") { cloud.copyAssistantInstructions() }
                Spacer()
                if cloud.authorized {
                    Button("重新授权") { cloud.restartAuthorization() }.disabled(!cloud.canRestartAuthorization)
                }
                Link("管理服务器", destination: URL(string: "https://cloud.digitalocean.com/droplets")!)
            }.font(.callout)
        }.padding(24).frame(width: 480, height: 640).background(Color(NSColor.windowBackgroundColor))
            .task { cloud.start() }
            .onDisappear { cloud.close() }
            .onChange(of: cloud.profile?.id) { _ in if cloud.profile != nil { saveProfile() } }
    }
    private var accountSection: some View {
        VStack(alignment: .leading, spacing: 12) {
            if cloud.intent == .create && cloud.authorized {
                Text("新建需要另外授权创建权限；现有服务器接入只使用读取权限。").font(.callout).foregroundStyle(.secondary)
            }
            Picker("授权浏览器", selection: $cloud.browser) { ForEach(cloud.browsers) { Text($0.title).tag($0) } }
                .disabled(cloud.working && !cloud.authorizationPending)
            if cloud.authorizationPending && cloud.authorizationPageReady {
                Text("在浏览器中找到“Clearline for macOS”，确认应用权限。登录 DigitalOcean 控制台本身不会完成这一步。").font(.callout)
                TimelineView(.periodic(from: .now, by: 1)) { context in
                    let seconds = max(0, Int((cloud.authorizationExpires ?? context.date).timeIntervalSince(context.date)))
                    Text(seconds > 0 ? "本次授权剩余 \(seconds / 60):\(String(format: "%02d", seconds % 60))" : "本次授权已过期，请重新授权")
                        .font(.caption).foregroundStyle(.secondary).monospacedDigit()
                }
                HStack {
                    Button("在所选浏览器打开") { cloud.reopenAuthorization() }
                    Button("重新发起授权") { cloud.restartAuthorization() }
                }.controlSize(.large)
            } else if cloud.accountLocked {
                action("解锁账号", enabled: !cloud.working) { cloud.unlockAccount() }
            } else if !cloud.authorizationPending {
                action(cloud.intent == .create ? "授权新建服务器权限" : "登录 DigitalOcean", enabled: !cloud.working) { cloud.command("login") }
            }
        }
    }
    @ViewBuilder private var existingSection: some View {
        if !cloud.servers.isEmpty {
            Picker("2 · 选择已有服务器", selection: Binding(get: { cloud.selectedServerID }, set: cloud.selectServer)) {
                Text("请选择服务器").tag(Optional<Int64>.none)
                ForEach(cloud.servers) { Text($0.name).tag(Optional($0.id)) }
            }.disabled(cloud.working)
        }
        if let server = cloud.selectedServer {
            if cloud.pairingCommand != nil {
                Text(server.name + " · " + server.price).font(.callout.weight(.medium))
            } else { VStack(alignment: .leading, spacing: 8) {
                Text(server.name).font(.headline)
                Text(server.detail).font(.callout).foregroundStyle(.secondary)
                Text(server.price).font(.callout.weight(.medium))
                Text("复用现有 Hysteria 2 服务，读取连接不会新建、升配或修改服务器。").font(.caption).foregroundStyle(.secondary)
            }.padding(16).frame(maxWidth: .infinity, alignment: .leading)
                .background(RoundedRectangle(cornerRadius: 12).fill(Color.primary.opacity(0.045))) }
            if !server.canRead { Text("服务器尚未运行或没有可用公网地址，请先在官网确认。").font(.callout).foregroundStyle(.orange) }
            if cloud.pairingCommand != nil {
                VStack(alignment: .leading, spacing: 10) {
                    Text("3 · 通过官方控制台配对").font(.headline)
                    Text("复制读取命令并打开控制台 → 粘贴运行 → 复制返回的 CLEARLINE1 连接码 → 回到这里粘贴。").font(.callout)
                    Text("命令只读取已有配置；连接码已加密，仅本次配对的 Mac 能读取。").font(.caption).foregroundStyle(.secondary)
                    Button("复制命令并打开控制台") { cloud.copyPairingAndOpenConsole() }.controlSize(.large)
                    action("粘贴连接码并保存", enabled: !cloud.working && !app.connected && !app.busy) { cloud.pastePairingCode() }.keyboardShortcut("v").help("也可按 ⌘V 粘贴并保存本次加密连接码")
                    Button("重新读取连接") { cloud.command("pairExisting") }.disabled(!cloud.canPair || app.connected || app.busy)
                }
            } else {
                action("读取并保存已有连接", enabled: cloud.canPair && !app.connected && !app.busy) { cloud.command("pairExisting") }
            }
        }
        if app.connected { Text("当前连接保持在线。需要更换服务器时，请先断开连接。").font(.caption).foregroundStyle(.secondary) }
        Button("刷新已有服务器") { cloud.command("servers") }.disabled(cloud.working)
    }
    @ViewBuilder private var createSection: some View {
        if let plan = cloud.plan, !cloud.hasDeployment {
            VStack(alignment: .leading, spacing: 10) {
                Text("新增服务器 · " + plan.location).font(.headline)
                Text(String(format: "US$%.2f / 月", plan.monthly)).font(.system(size: 28, weight: .semibold))
                Text(String(format: "官网标示小时价 US$%.5f · %@", plan.hourly, plan.size)).font(.caption).foregroundStyle(.secondary)
                Text("这是额外的持续费用；已有服务器和套餐保持原样。").font(.caption).foregroundStyle(.secondary)
            }.padding(16).frame(maxWidth: .infinity, alignment: .leading).background(RoundedRectangle(cornerRadius: 12).fill(Color.primary.opacity(0.045)))
            Toggle("我确认由自己的 DigitalOcean 账号承担新增服务器费用", isOn: $cloud.consent).disabled(cloud.working || app.connected)
            action("确认费用并新建", enabled: cloud.canDeploy && !app.connected && !app.busy) { cloud.command("deploy") }
            Button("刷新新建方案") { cloud.command("plan") }.disabled(cloud.working)
        } else if cloud.hasDeployment {
            action("继续检查上次部署", enabled: !cloud.working && !app.connected && !app.busy) { cloud.command("resume") }
        } else {
            Button("获取新建方案") { cloud.command("plan") }.disabled(cloud.working)
        }
    }
    private var importSection: some View {
        VStack(alignment: .leading, spacing: 12) {
            Text("粘贴 Hysteria 2 链接或 WireGuard / AmneziaWG 配置").font(.callout)
            TextEditor(text: $importText).font(.system(size: 11, design: .monospaced)).frame(height: 150)
                .padding(8).background(RoundedRectangle(cornerRadius: 8).fill(Color(NSColor.textBackgroundColor))).accessibilityLabel("已有连接配置")
            action("保存已有连接", enabled: !importText.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && !saving && !app.connected && !app.busy) {
                saving = true
                Task {
                    do { try await app.add(name: "我的服务器", config: importText); importText = ""; cloud.markSaved() }
                    catch { cloud.profileSaveFailed(error) }
                    saving = false
                }
            }
            Button("导入加密 .clearline 文件…") { app.importAfterSetup = true; dismiss() }.disabled(app.connected || app.busy)
        }
    }
    private func saveProfile() {
        guard let profile = cloud.profile, !saving else { return }
        saving = true
        Task {
            do { try await app.importCloudProfile(profile); cloud.markSaved() }
            catch { cloud.profileSaveFailed(error) }
            saving = false
        }
    }
    private func action(_ title: String, enabled: Bool = true, perform: @escaping () -> Void) -> some View {
        Button(action: perform) { Text(title).font(.body.weight(.medium)).frame(maxWidth: .infinity, minHeight: 42).foregroundStyle(.white).background(RoundedRectangle(cornerRadius: 10).fill(accent.opacity(enabled ? 1 : 0.4))) }
            .buttonStyle(InteractiveStyle(surface: .segment)).disabled(!enabled)
    }
}

@MainActor struct NetworkCheckView: View {
    @EnvironmentObject var app: AppModel
    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(app.networkPreflight?.title ?? "连接前检查").font(.title2.weight(.semibold))
            if let report = app.networkPreflight {
                Text("分流和 DNS 保护需要独立接管网络。请先停用其他系统代理或 VPN，再重新检查。").fixedSize(horizontal: false, vertical: true)
                if report.serverUsesVPNRoute, let address = report.bypassAddress {
                    Text("服务器入口：" + address).font(.callout).textSelection(.enabled)
                    Button("复制入口直连地址") { NSPasteboard.general.clearContents(); NSPasteboard.general.setString(address, forType: .string) }
                    if report.shadowrocketDetected { Text("Shadowrocket：配置 → 通用 → TUN 排除路由，添加这个单地址例外并更新配置。").font(.callout).foregroundStyle(.secondary) }
                }
            }
            HStack {
                Button("暂不连接") { app.cancelNetworkCheck() }
                Spacer()
                Button("重新检查") { app.connect() }.buttonStyle(.borderedProminent)
            }.controlSize(.large)
        }.padding(24).frame(width: 460)
    }
}
