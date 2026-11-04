import AppKit
import SwiftUI
import Security
import ClearlineKit

enum CloudVault {
    static let service = "org.clearline.digitalocean"
    static func load() throws -> Data? {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "state", kSecReturnData as String: true]
        var value: CFTypeRef?
        let result = SecItemCopyMatching(query as CFDictionary, &value)
        if result == errSecItemNotFound { return nil }
        guard result == errSecSuccess, let data = value as? Data, data.count <= 2_000_000 else { throw CocoaError(.fileReadNoPermission) }
        return data
    }
    static func save(_ data: Data) throws {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "state"]
        let result = SecItemUpdate(query as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        if result == errSecItemNotFound {
            var item = query; item[kSecValueData as String] = data
            item[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            guard SecItemAdd(item as CFDictionary, nil) == errSecSuccess else { throw CocoaError(.fileWriteNoPermission) }
        } else if result != errSecSuccess { throw CocoaError(.fileWriteNoPermission) }
    }
}

struct CloudPlanViewData: Decodable, Equatable {
    let id: String
    let region: String
    let size: String
    let monthly: Double
    let hourly: Double
    let transfer: Double
    let expires: Int64
    var location: String { region.hasPrefix("sfo") ? "美国 · 旧金山" : "美国 · 纽约" }
}

@MainActor final class CloudSetupModel: ObservableObject {
    @Published var authorized = false
    @Published var working = false
    @Published var hasDeployment = false
    @Published var plan: CloudPlanViewData?
    @Published var consent = false
    @Published var message = "登录自己的 DigitalOcean 账号，配置你的美国服务器。"
    @Published var error: String?
    @Published var profile: Profile?
    @Published var saved = false
    private var child: JSONChild?
    private var closed = false
    private let saveState: (Data) throws -> Void
    private let loadState: () throws -> Data?
    private let openAuthorization: (URL) -> Bool
    private let persistsStatus: Bool
    var sendOverride: (([String: Any]) throws -> Void)?

    init(load: @escaping () throws -> Data? = CloudVault.load,
         save: @escaping (Data) throws -> Void = CloudVault.save,
         open: @escaping (URL) -> Bool = { NSWorkspace.shared.open($0) }, writeStatus: Bool = true) {
        loadState = load; saveState = save; openAuthorization = open; persistsStatus = writeStatus
    }
    static func validAuthorizationURL(_ raw: String) -> URL? {
        guard let url = URL(string: raw), url.scheme == "https", url.host == "cloud.digitalocean.com",
              url.path == "/v1/oauth/authorize", url.user == nil, url.password == nil, url.port == nil else { return nil }
        return url
    }
    var canDeploy: Bool { authorized && !working && consent && !hasDeployment && plan != nil && (plan?.expires ?? 0) > Int64(Date().timeIntervalSince1970) }
    func start() {
        guard child == nil, !closed, sendOverride == nil else { return }
        do {
            var request: [String: Any] = [:]
            if let data = try loadState() { request["state"] = try JSONSerialization.jsonObject(with: data) }
            let process = JSONChild(executable: CoreRPC.coreURL, arguments: ["cloud"])
            child = process
            process.onEvent = { [weak self] event in self?.accept(event) }
            process.onExit = { [weak self] _ in
                guard let self, !self.closed else { return }
                self.working = false; self.child = nil; self.error = "配置进程已停止，可关闭窗口后重试。"
            }
            try process.start(); try process.send(request)
        } catch { self.error = "无法读取本机云配置，请允许钥匙串访问后重试。" }
    }
    func send(_ request: [String: Any]) throws {
        if let override = sendOverride { try override(request) }
        else if let child { try child.send(request) }
        else { throw CocoaError(.executableNotLoadable) }
    }
    func command(_ action: String) {
        guard !working else { return }
        if action == "deploy" && !canDeploy { return }
        error = nil; working = true
        if action == "login" { message = "请在 DigitalOcean 官网登录并授权，完成后返回轻连。" }
        if action == "plan" { plan = nil; consent = false; message = "正在读取可用服务器和价格" }
        if action == "resume" { profile = nil; saved = false }
        do {
            var request: [String: Any] = ["action": action]
            if action == "deploy" { request["id"] = plan?.id }
            try send(request)
        } catch { working = false; self.error = "无法发起操作，请关闭窗口后重试。" }
        writeAssistantStatus()
    }
    func accept(_ event: [String: Any]) {
        switch event["event"] as? String {
        case "cloudState":
            do {
                guard let state = event["state"], let id = event["id"] as? String else { throw CocoaError(.fileReadCorruptFile) }
                try saveState(JSONSerialization.data(withJSONObject: state))
                try send(["action": "stored", "id": id])
            } catch { child?.stop(); working = false; self.error = "配置未能保存到本机钥匙串，后续云操作已停止。" }
        case "cloudAuthorize":
            guard let raw = event["url"] as? String, let url = Self.validAuthorizationURL(raw), openAuthorization(url) else {
                child?.stop(); working = false; error = "无法打开 DigitalOcean 官方授权页面。"; return
            }
        case "cloudReady":
            authorized = event["authorized"] as? Bool == true
            hasDeployment = event["hasDeployment"] as? Bool == true
            if authorized { command(hasDeployment ? "resume" : "plan") }
        case "cloudAuthorized": authorized = true; message = "已授权，正在读取服务器方案"
        case "cloudPlan":
            if let raw = event["plan"], let data = try? JSONSerialization.data(withJSONObject: raw), let plan = try? JSONDecoder().decode(CloudPlanViewData.self, from: data), plan.monthly > 0, plan.hourly > 0 {
                self.plan = plan; consent = false; working = false; message = "确认方案后，为你的账号部署独立服务器。"
            } else { working = false; error = "服务器价格无效，请重新获取方案。" }
        case "cloudExisting": hasDeployment = true; working = false; message = "已有部署记录，可继续检查。"
        case "cloudProgress": message = event["message"] as? String ?? "正在配置服务器"
        case "cloudDeployed":
            hasDeployment = true; working = true
            guard let raw = event["profile"], let data = try? JSONSerialization.data(withJSONObject: raw), let incoming = try? JSONDecoder().decode(Profile.self, from: data) else {
                working = false; error = "服务器连接数据无效，请继续检查已有服务器。"; break
            }
            profile = incoming
            message = "服务器与美国出口验证完成，正在保存连接。"
        case "cloudError", "error":
            working = false; error = event["message"] as? String ?? "配置未完成，请重试。"
            consent = false
            hasDeployment = event["hasDeployment"] as? Bool ?? hasDeployment
        default: break
        }
        writeAssistantStatus()
    }
    func close() { closed = true; child?.stop(); child = nil }
    func markSaved() { saved = true; working = false; message = "你的美国连接已准备好。"; writeAssistantStatus() }
    func copyAssistantInstructions() {
        let text = """
        请按照 https://github.com/wenh09/clearline 的 AGENTS.md 帮我完成 Clearline 安装和 DigitalOcean 自助部署。
        请打开应用的“设置 DigitalOcean”，让我本人登录和授权。创建服务器前，向我展示应用读取的实际价格，并等待我在应用中确认费用。
        你可以读取 ~/Library/Application Support/Clearline/setup-status.json 了解阶段和错误；该文件不包含令牌、服务器地址或连接密钥。
        凭据由应用保存在本机钥匙串，请通过应用完成部署和恢复。部署结果不确定时先核对已有服务器，保留正在使用的联网连接。
        """
        NSPasteboard.general.clearContents(); NSPasteboard.general.setString(text, forType: .string)
    }
    private func writeAssistantStatus() {
        guard persistsStatus else { return }
        // Deliberately exclude account IDs, IPs, URLs, profile contents and raw
        // provider errors. AI helpers can inspect progress without credentials.
        let status: [String: Any] = ["version": "0.3.0", "authorized": authorized, "working": working, "hasDeployment": hasDeployment, "connectionSaved": saved, "needsAttention": error != nil, "monthlyUSD": plan?.monthly ?? 0, "region": plan?.region ?? "", "updatedAt": ISO8601DateFormatter().string(from: Date())]
        guard let data = try? JSONSerialization.data(withJSONObject: status, options: [.sortedKeys]), let parent = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?.appendingPathComponent("Clearline") else { return }
        do {
            try FileManager.default.createDirectory(at: parent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let file = parent.appendingPathComponent("setup-status.json")
            try data.write(to: file, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
        } catch { }
    }
}

@MainActor struct CloudSetupView: View {
    @EnvironmentObject var app: AppModel
    @Environment(\.dismiss) private var dismiss
    @StateObject private var cloud: CloudSetupModel
    init(model: CloudSetupModel? = nil) { _cloud = StateObject(wrappedValue: model ?? CloudSetupModel()) }
    private let accent = Color(red: 0.09, green: 0.55, blue: 0.47)
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack {
                Image(systemName: "cloud.fill").foregroundStyle(accent)
                Text("设置 DigitalOcean").font(.title2.weight(.semibold))
                Spacer()
                Button { dismiss() } label: { Image(systemName: "xmark").frame(width: 40, height: 40) }
                    .buttonStyle(InteractiveStyle(surface: .segment)).accessibilityLabel("关闭设置")
            }
            Text(cloud.message).font(.body).foregroundStyle(.secondary).fixedSize(horizontal: false, vertical: true)
            if let error = cloud.error { Text(error).font(.callout).foregroundStyle(.orange).fixedSize(horizontal: false, vertical: true) }
            if cloud.working { HStack { ProgressView().controlSize(.small); Text("处理中…").foregroundStyle(.secondary) } }
            if let plan = cloud.plan, !cloud.hasDeployment {
                VStack(alignment: .leading, spacing: 12) {
                    Text(plan.location).font(.headline)
                    Text(String(format: "US$%.2f / 月", plan.monthly)).font(.system(size: 28, weight: .semibold)).monospacedDigit()
                    Text(String(format: "官网标示小时价 US$%.5f · %@", plan.hourly, plan.size)).font(.caption).foregroundStyle(.secondary).monospacedDigit()
                    Text("DigitalOcean 独立计费，完整费用和账单以官网为准。").font(.caption).foregroundStyle(.secondary)
                }.padding(18).frame(maxWidth: .infinity, alignment: .leading)
                    .background(RoundedRectangle(cornerRadius: 14).fill(Color.primary.opacity(0.045)))
                Toggle("我确认由自己的 DigitalOcean 账号承担服务器费用", isOn: $cloud.consent).font(.callout).disabled(cloud.working)
                action("确认费用并部署", enabled: cloud.canDeploy) { cloud.command("deploy") }
                Button("刷新方案") { cloud.command("plan") }.buttonStyle(InteractiveStyle(surface: .segment)).frame(minHeight: 40).disabled(cloud.working)
            } else if cloud.saved {
                Label("连接已保存到本机", systemImage: "checkmark.circle.fill").foregroundStyle(accent)
                action("完成") { dismiss() }
            } else if cloud.hasDeployment {
                action("继续检查已有服务器", enabled: !cloud.working) { cloud.command("resume") }
            } else if !cloud.authorized {
                action("登录 DigitalOcean", enabled: !cloud.working) { cloud.command("login") }
            } else {
                action("重新获取服务器方案", enabled: !cloud.working) { cloud.command("plan") }
            }
            HStack(spacing: 12) {
                Button("让 AI 协助") { cloud.copyAssistantInstructions() }.buttonStyle(InteractiveStyle(surface: .segment)).frame(minHeight: 40).help("复制可交给 Codex 或 Claude Code 的协作说明")
                Spacer()
                Link("管理服务器与账单", destination: URL(string: "https://cloud.digitalocean.com/droplets")!).font(.callout)
            }
            if cloud.authorized {
                Button("重新登录授权") { cloud.command("login") }.buttonStyle(InteractiveStyle(surface: .segment)).frame(minHeight: 40).disabled(cloud.working)
            }
        }.padding(24).frame(width: 440).background(Color(NSColor.windowBackgroundColor))
            .task { cloud.start() }
            .onDisappear { cloud.close() }
            .onChange(of: cloud.profile?.id) { _ in
                guard let profile = cloud.profile else { return }
                Task {
                    do { try await app.importCloudProfile(profile); cloud.markSaved() }
                    catch { cloud.working = false; cloud.error = "服务器已部署，连接尚未保存。请允许钥匙串访问后继续检查。" }
                }
            }
    }
    private func action(_ title: String, enabled: Bool = true, perform: @escaping () -> Void) -> some View {
        Button(action: perform) { Text(title).font(.body.weight(.medium)).frame(maxWidth: .infinity, minHeight: 44).foregroundStyle(.white).background(RoundedRectangle(cornerRadius: 10).fill(accent.opacity(enabled ? 1 : 0.4))) }
            .buttonStyle(InteractiveStyle(surface: .segment)).disabled(!enabled)
    }
}
