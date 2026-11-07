import AppKit
import SwiftUI
import UniformTypeIdentifiers
import ClearlineKit

@main struct ClearlineApp: App {
    @StateObject private var model = AppModel()
    init() {
        // A data-only command for provisioning one's own local app; it never
        // connects, changes network preferences, or prints secret contents.
        if CommandLine.arguments.count == 3 && CommandLine.arguments[1] == "--import-profiles" {
            do {
                let data = try Data(contentsOf: URL(fileURLWithPath: CommandLine.arguments[2]))
                guard data.count <= 1_048_576 else { throw CocoaError(.fileReadTooLarge) }
                let incoming = try JSONDecoder().decode([Profile].self, from: data)
                guard incoming.count <= 32 else { throw CocoaError(.fileReadTooLarge) }
                var saved = try KeychainStore.load()
                for var profile in incoming where !saved.contains(where: { $0.config == profile.config }) {
                    let core = Process(); core.executableURL = CoreRPC.coreURL; core.arguments = ["validate"]
                    let input = Pipe(); core.standardInput = input; core.standardOutput = FileHandle.nullDevice; core.standardError = FileHandle.nullDevice
                    try core.run(); var request = try JSONSerialization.data(withJSONObject: ["profile": CoreRPC.dictionary(profile)]); request.append(10)
                    try input.fileHandleForWriting.write(contentsOf: request); try input.fileHandleForWriting.close(); core.waitUntilExit()
                    guard core.terminationStatus == 0 else { throw CocoaError(.fileReadCorruptFile) }
                    profile.id = UUID().uuidString; saved.append(profile)
                }
                try KeychainStore.save(saved); print("Imported \(incoming.count) profiles into this Mac's keychain."); exit(0)
            } catch { fputs("Profile import failed.\n", stderr); exit(1) }
        }
    }
    var body: some Scene {
        WindowGroup("轻连", id: "main") {
            MainView().environmentObject(model).frame(width: 360, height: 660)
        }
        .windowResizability(.contentSize)
        .windowStyle(.hiddenTitleBar)
        .commands {
            CommandGroup(replacing: .newItem) {
                Button("导入配置…") { model.openTransfer(importing: true) }
                    .keyboardShortcut("o").disabled(model.connected || model.busy || model.loadingProfiles)
                Button("导出配置…") { model.openTransfer(importing: false) }
                    .disabled(model.profiles.isEmpty || model.busy || model.storageError)
            }
            CommandMenu("代理范围") {
                RoutingModeMenu().environmentObject(model)
            }
            CommandMenu("工具") {
                Button("连接设置…") { model.showCloudSetup = true }.disabled(model.busy || model.loadingProfiles)
                Button("恢复网络") { model.recover() }.disabled(model.connected || model.busy)
                Button("安装/更新网络组件…") { TunnelService.openInstaller() }.disabled(model.packetTunnelActive || model.busy)
                Button("复制终端代理命令") { model.copyTerminalProxy() }.disabled(!model.connected)
            }
            CommandGroup(replacing: .appTermination) {
                Button("退出轻连") { model.quit() }.keyboardShortcut("q")
            }
        }
        MenuBarExtra {
            MenuContents().environmentObject(model)
        } label: {
            Image(systemName: model.connected ? "network.badge.shield.half.filled" : "network")
        }
    }
}

private struct MenuContents: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.openWindow) var openWindow

    var body: some View {
        Text(model.statusTitle + " · " + model.routingMode.title)
        Button(model.connected || model.busy ? "断开连接" : model.primaryActionTitle) { model.performPrimaryAction() }
            .disabled(model.loadingProfiles || model.state == .disconnecting)
        Divider()
        Menu("代理范围") { RoutingModeMenu().environmentObject(model) }
        Button("打开轻连") { openWindow(id: "main"); NSApp.activate(ignoringOtherApps: true) }
        Divider()
        Button("退出轻连") { model.quit() }
    }
}

private struct RoutingModeMenu: View {
    @EnvironmentObject var model: AppModel
    var body: some View {
        ForEach(AppModel.RoutingMode.allCases) { mode in
            Toggle(mode.title, isOn: Binding(
                get: { model.routingMode == mode },
                set: { if $0 { model.setRoutingMode(mode) } }
            )).disabled(model.busy || model.loadingProfiles)
        }
    }
}

struct MainView: View {
    @EnvironmentObject var model: AppModel
    private let accent = Color(red: 0.09, green: 0.55, blue: 0.47)

    var body: some View {
        VStack(spacing: 0) {
            Text("轻连")
                .font(.system(size: 19, weight: .semibold))
                .padding(.top, 18)
            Spacer(minLength: 0)
            Button { model.performPrimaryAction() } label: {
                ZStack {
                    Circle().fill(accent.opacity(model.connected ? 0.08 : 0.025))
                        .frame(width: 104, height: 104)
                    Circle().fill(model.connected ? accent : Color(NSColor.controlBackgroundColor))
                        .frame(width: 88, height: 88)
                        .shadow(color: accent.opacity(model.connected ? 0.18 : 0.04), radius: 16, y: 7)
                        .shadow(color: .black.opacity(0.04), radius: 2, y: 1)
                    if model.busy || model.loadingProfiles {
                        ProgressView().controlSize(.large).tint(accent)
                    } else {
                        Image(systemName: model.storageError ? "lock" : model.profiles.isEmpty ? "plus" : "power")
                            .font(.system(size: 30, weight: .light))
                            .foregroundStyle(model.connected ? .white : accent)
                    }
                }
                .contentShape(Rectangle())
            }
            .buttonStyle(InteractiveStyle(surface: .power))
            .disabled(model.loadingProfiles || model.state == .disconnecting)
            .accessibilityLabel(model.primaryActionTitle)
            .padding(.bottom, 10)

            Text(model.statusTitle)
                .font(.system(size: 24, weight: .medium))
                .accessibilityAddTraits(.updatesFrequently)
            if let profile = model.selected {
                Text(profile.name).font(.system(size: 12)).foregroundStyle(.secondary).lineLimit(2).padding(.horizontal, 24).padding(.top, 6)
            }
            if let message = model.actionMessage {
                Text(message).font(.system(size: 12)).foregroundStyle(.secondary)
                    .multilineTextAlignment(.center).fixedSize(horizontal: false, vertical: true)
                    .padding(.horizontal, 28).padding(.top, 8)
            }
            ConnectionFeedbackView().environmentObject(model)
                .padding(.top, 8)
            RoutingModeControl().environmentObject(model)
                .padding(.top, 10)
            if model.connected {
                Text(model.packetTunnelActive ? "DNS / UDP 已接管" : "系统代理 · DNS / UDP 未接管")
                    .font(.system(size: 11)).foregroundStyle(model.packetTunnelActive ? accent : .secondary).padding(.top, 5)
            }
            NetworkActionsView().environmentObject(model)
                .padding(.top, 4)
            ExitVerificationView().environmentObject(model)
            SpeedResultsView().environmentObject(model)
                .padding(.top, 6)
            Spacer(minLength: 0)
        }
        .padding(.bottom, 8)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color(NSColor.windowBackgroundColor))
        .sheet(isPresented: $model.showImport) { ImportView().environmentObject(model) }
        .sheet(isPresented: $model.showTransfer) { TransferView(importing: model.transferImport).environmentObject(model) }
        .sheet(isPresented: $model.showCloudSetup, onDismiss: {
            if model.importAfterSetup { model.importAfterSetup = false; model.openTransfer(importing: true) }
            else if model.connectAfterSetup { model.connectAfterSetup = false; model.connect() }
        }) { ConnectionSetupView().environmentObject(model) }
        .sheet(isPresented: $model.showNetworkCheck) { NetworkCheckView().environmentObject(model) }
        .sheet(isPresented: $model.needsTunnelApproval) {
            VStack(alignment: .leading, spacing: 16) {
                Text("安装系统隧道组件").font(.title2.weight(.semibold))
                Text("全局隧道需要创建虚拟网卡，接管公网 TCP、UDP 与系统 DNS。请打开网络组件安装包，在 macOS 安装器中确认管理员权限，再返回轻连。更新组件前请先断开全局隧道。")
                    .fixedSize(horizontal: false, vertical: true)
                Text("连接凭据不会交给后台服务。断开时会恢复原 DNS 和路由；AI 分流仍可使用系统代理。")
                    .font(.callout).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
                HStack {
                    Button("稍后") { model.needsTunnelApproval = false }
                    Spacer()
                    Button("打开安装器") { TunnelService.openInstaller() }
                    Button("安装完成，继续连接") { model.finishTunnelInstallation() }
                }
            }.padding(24).frame(width: 460)
        }
        .onOpenURL { url in
            guard !model.connected, !model.busy else { return }
            if url.pathExtension == "clearline" {
                model.transferFile = url; model.openTransfer(importing: true)
            } else if let data = try? Data(contentsOf: url), data.count <= 1_048_576 {
                model.importText = String(decoding: data, as: UTF8.self)
                model.importName = url.deletingPathExtension().lastPathComponent
                model.showImport = true
            }
        }
    }
}

private struct RoutingModeControl: View {
    @EnvironmentObject var model: AppModel
    private let accent = Color(red: 0.09, green: 0.55, blue: 0.47)
    var body: some View {
        HStack(spacing: 4) {
            ForEach(AppModel.RoutingMode.allCases) { mode in
                Button { model.setRoutingMode(mode) } label: {
                    Text(mode.title).font(.system(size: 12, weight: .medium))
                        .foregroundStyle(model.routingMode == mode ? accent : .secondary)
                        .frame(maxWidth: .infinity, minHeight: 40)
                        .background {
                            if model.routingMode == mode {
                                RoundedRectangle(cornerRadius: 8)
                                    .fill(Color(NSColor.controlBackgroundColor))
                                    .shadow(color: .black.opacity(0.04), radius: 2, y: 1)
                            }
                        }
                        .contentShape(RoundedRectangle(cornerRadius: 8))
                }.buttonStyle(InteractiveStyle(surface: .segment))
                    .accessibilityLabel(mode.title)
                    .accessibilityValue(model.routingMode == mode ? "已选择" : "未选择")
                    .help(mode.help)
                    .disabled(model.busy || model.loadingProfiles)
            }
        }.padding(4)
            .background(RoundedRectangle(cornerRadius: 12).fill(Color.primary.opacity(0.045)))
            .frame(width: 252)
            .accessibilityElement(children: .contain).accessibilityLabel("代理范围")
    }
}

private struct ConnectionFeedbackView: View {
    @EnvironmentObject var model: AppModel
    private let accent = Color(red: 0.09, green: 0.55, blue: 0.47)
    private let phases: [(ConnectionProgress.Phase, String)] = [(.server, "连接服务器"), (.checking, "验证网络"), (.applying, "接入 Mac")]
    var body: some View {
        TimelineView(.animation(minimumInterval: 0.1, paused: !model.busy)) { _ in
            let now = ProcessInfo.processInfo.systemUptime
            VStack(spacing: 8) {
                if let progress = model.connectionProgress {
                    if progress.phase == .restoring || progress.phase == .waiting {
                        HStack(spacing: 6) {
                            if model.busy { ProgressView().controlSize(.mini) }
                            Text(progress.phase == .restoring ? "恢复原网络" : model.retryUntil == nil ? "等待网络恢复" : "将在 \(Int(ceil(max(0, (model.retryUntil ?? now) - now)))) 秒后重试")
                        }.font(.system(size: 11)).foregroundStyle(.secondary)
                    } else {
                        HStack(spacing: 8) {
                            ForEach(phases, id: \.0) { phase, title in
                                VStack(spacing: 4) {
                                    HStack(spacing: 3) {
                                        Image(systemName: progress.hasFinished(phase) || (phase == .applying && model.proxyRetainedDuringRecovery) ? "checkmark.circle.fill" : progress.phase == phase ? "circle.dotted" : "circle")
                                        Text(title)
                                    }.foregroundStyle(progress.hasFinished(phase) || progress.phase == phase ? accent : .secondary)
                                    Text(phase == .applying && model.proxyRetainedDuringRecovery ? "已保持" : progress.hasFinished(phase) || progress.phase == phase ? String(format: "%.1f s", progress.duration(of: phase, at: now)) : "—")
                                        .monospacedDigit().foregroundStyle(.secondary)
                                }.font(.system(size: 10)).frame(maxWidth: .infinity)
                            }
                        }
                    }
                    HStack(spacing: 16) {
                        Text("\(progress.kind == .disconnect ? "断开" : progress.kind == .switching ? "切换" : progress.kind == .reconnect ? "重连" : "总连接") \(String(format: "%.1f s", progress.elapsed(at: now)))")
                        if model.connected {
                            Text(model.latency.map { "HTTPS 响应 \($0) ms" } ?? "正在检查响应")
                                .help("通过当前隧道请求轻量 HTTPS 检测地址，包含建连与服务响应时间；每 30 秒更新。")
                        }
                    }.font(.system(size: 11)).monospacedDigit().foregroundStyle(.secondary)
                } else {
                    Text("连接后显示阶段耗时和网络响应").font(.system(size: 11)).foregroundStyle(.tertiary)
                }
            }.frame(height: 62).padding(.horizontal, 24)
        }
    }
}

private struct NetworkActionsView: View {
    @EnvironmentObject var model: AppModel
    private let accent = Color(red: 0.09, green: 0.55, blue: 0.47)
    var body: some View {
        HStack(spacing: 8) {
            Button { model.verifyExit() } label: {
                HStack(spacing: 4) {
                    if model.isVerifyingExit { ProgressView().controlSize(.mini) }
                    Text(model.isVerifyingExit ? "验证中…" : model.exitVerification == nil ? "验证出口 IP" : "重新验证 IP")
                }.frame(maxWidth: .infinity, minHeight: 40).contentShape(Rectangle())
            }.disabled(!model.connected || model.isVerifyingExit || model.isSpeedTesting)
            Button { model.isSpeedTesting ? model.stopSpeedTest() : model.startSpeedTest() } label: {
                HStack(spacing: 4) {
                    if model.isSpeedTesting { ProgressView().controlSize(.mini) }
                    Text(model.isCancellingSpeed ? "停止中…" : model.isSpeedTesting ? "停止测速" : "网络测速")
                }.frame(maxWidth: .infinity, minHeight: 40).contentShape(Rectangle())
            }.disabled(model.busy || model.isVerifyingExit || model.isCancellingSpeed)
                .help("匿名检测 Claude 和普通网络响应，并下载最多 5 MB 样本；可随时停止。")
        }.font(.system(size: 12, weight: .medium)).foregroundStyle(accent)
            .buttonStyle(InteractiveStyle()).frame(width: 280)
    }
}

private struct ExitVerificationView: View {
    @EnvironmentObject var model: AppModel
    var body: some View {
        VStack(spacing: 3) {
            if let result = model.exitVerification {
                Text(result.ip).font(.system(size: 13, weight: .medium, design: .monospaced))
                    .lineLimit(1).minimumScaleFactor(0.7).textSelection(.enabled)
                Text(result.region + " · " + result.verifiedAt.formatted(date: .omitted, time: .shortened) + " 已验证")
                    .font(.system(size: 10)).foregroundStyle(result.country == "US" ? Color.secondary : Color.orange)
            } else if let error = model.exitError {
                Text(error).font(.system(size: 11)).foregroundStyle(.orange).lineLimit(2)
            }
        }.padding(.horizontal, 24).frame(height: 36)
    }
}

private struct SpeedResultsView: View {
    @EnvironmentObject var model: AppModel
    private func routeTitle(_ route: String) -> String { route == "tunnel" ? "代理" : "直连" }
    private func siteRow(_ title: String, sample: SiteSpeed?, fallbackRoute: String) -> some View {
        HStack {
            VStack(alignment: .leading, spacing: 2) {
                Text(title).font(.system(size: 11, weight: .medium))
                Text(sample.map { siteDetail($0) } ?? routeTitle(fallbackRoute))
                    .font(.system(size: 9)).foregroundStyle(sample?.limited == true || (sample?.ok == false && sample?.skipped == false) ? Color.orange : Color.secondary)
                    .lineLimit(1)
            }
            Spacer(minLength: 8)
            Text(sample.map { $0.skipped ? "待连接" : $0.ok ? "\($0.responseMs) ms" : "未连通" } ?? "—")
                .font(.system(size: 12, weight: .medium)).monospacedDigit()
                .foregroundStyle(sample?.limited == true ? Color.orange : Color.primary)
        }.frame(height: 28)
    }
    var body: some View {
        VStack(spacing: 4) {
            siteRow("Claude 网站", sample: model.speedReport?.claude, fallbackRoute: "tunnel")
                .help("匿名 HEAD 请求 Claude 网站，显示到首字节的响应时间。403 表示验证页或受限，不代表登录后的可用性。")
            siteRow("普通网络", sample: model.speedReport?.ordinary, fallbackRoute: model.connected && model.routingMode == .all ? "tunnel" : "direct")
                .help("主数字为 3 次首次访问的响应中位数；连续访问时间来自实际复用连接。按当前范围走代理或直连。")
            HStack {
                VStack(alignment: .leading, spacing: 2) {
                    Text("下载采样").font(.system(size: 11, weight: .medium))
                    Text(downloadDetail).font(.system(size: 9)).foregroundStyle(.secondary).lineLimit(1)
                }
                Spacer()
                Text(model.speedReport?.download.map { $0.ok ? String(format: "%.1f Mbps", $0.mbps) : "未测成" } ?? "—")
                    .font(.system(size: 12, weight: .medium)).monospacedDigit()
            }.frame(height: 28).help("最多 5 MB 的短时下载样本，整体速率包含建连和等待响应的耗时。Mbps 为每秒兆比特，结果不代表长期带宽。")
            if let error = model.speedError {
                Text(error).font(.system(size: 10)).foregroundStyle(.orange).lineLimit(1)
            } else if !model.speedStage.isEmpty {
                Text(model.speedStage).font(.system(size: 10)).foregroundStyle(.secondary)
            }
        }.padding(10).frame(width: 280)
            .background(RoundedRectangle(cornerRadius: 12).fill(Color.primary.opacity(0.035)))
    }
    private func siteDetail(_ sample: SiteSpeed) -> String {
        let route = routeTitle(sample.route)
        guard sample.ok, !sample.limited else { return route + " · " + sample.message }
        if let warm = sample.warmMs, (sample.warmSamples ?? 0) > 0 {
            return route + " · \(sample.samples ?? 1) 次中位 · 连续 \(warm) ms"
        }
        if (sample.samples ?? 0) > 1 { return route + " · 首次 \(sample.samples!) 次采样" }
        return route + " · " + sample.message
    }
    private var downloadDetail: String {
        if model.isSpeedTesting && model.speedBytes > 0 { return String(format: "%.1f / 5 MB", Double(model.speedBytes) / 1_000_000) }
        if let sample = model.speedReport?.download {
            return routeTitle(sample.route) + " · " + (sample.ok ? String(format: "5 MB · %.1f s · 含建连", Double(sample.durationMs)/1000) : sample.message)
        }
        return "最多 5 MB · " + (model.connected && model.routingMode == .all ? "代理" : "直连")
    }
}

private struct ImportView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    @State private var config = ""
    @State private var error = ""
    @State private var working = false

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text("导入配置").font(.system(size: 21, weight: .semibold))
            TextEditor(text: $config).font(.system(size: 11, design: .monospaced))
                .scrollContentBackground(.hidden).padding(10)
                .background(RoundedRectangle(cornerRadius: 10).fill(Color(NSColor.textBackgroundColor)))
                .frame(height: 160).accessibilityLabel("连接配置")
            if !error.isEmpty { Text(error).font(.system(size: 12)).foregroundStyle(.orange) }
            HStack {
                Spacer()
                Button("取消") { dismiss() }.keyboardShortcut(.cancelAction)
                Button(working ? "正在导入…" : "导入") {
                    working = true
                    Task {
                        do { try await model.add(name: model.importName, config: config); dismiss() }
                        catch { self.error = error.localizedDescription; working = false }
                    }
                }.buttonStyle(.borderedProminent).disabled(config.isEmpty || working).keyboardShortcut(.defaultAction)
            }
        }.padding(24).frame(width: 380)
        .onAppear { config = model.importText; model.importText = "" }
    }
}

private struct TransferView: View {
    @EnvironmentObject var model: AppModel
    @Environment(\.dismiss) var dismiss
    let importing: Bool
    @State private var password = ""
    @State private var confirm = ""
    @State private var error = ""
    @State private var working = false

    var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Text(importing ? "导入配置" : "导出配置").font(.system(size: 21, weight: .semibold))
            SecureField(importing ? "配置密码" : "设置密码，至少 10 个字符", text: $password).textFieldStyle(.roundedBorder)
            if !importing { SecureField("再次输入密码", text: $confirm).textFieldStyle(.roundedBorder) }
            if !error.isEmpty { Text(error).font(.system(size: 12)).foregroundStyle(.orange) }
            HStack {
                Spacer()
                Button("取消") { dismiss() }.keyboardShortcut(.cancelAction)
                Button(working ? "正在处理…" : importing ? "选择文件并导入…" : "导出…") { chooseFile() }
                    .buttonStyle(.borderedProminent)
                    .disabled(working || password.count < (importing ? 1 : 10) || (!importing && password != confirm))
                    .keyboardShortcut(.defaultAction)
            }
        }.padding(24).frame(width: 340)
    }
    private func chooseFile() {
        var url: URL?
        if importing, let file = model.transferFile { url = file; model.transferFile = nil }
        else if importing {
            let panel = NSOpenPanel()
            panel.allowedContentTypes = [UTType(filenameExtension: "clearline") ?? .data]
            if panel.runModal() == .OK { url = panel.url }
        } else {
            let panel = NSSavePanel(); panel.nameFieldStringValue = "轻连配置.clearline"
            if panel.runModal() == .OK { url = panel.url }
        }
        guard let url else { return }; working = true
        Task {
            do {
                if importing { try await model.importTransfer(password: password, url: url) }
                else { try await model.export(password: password, url: url) }
                dismiss()
            } catch { self.error = error.localizedDescription; working = false }
        }
    }
}
