import AppKit
import SwiftUI
import ClearlineKit

enum SetupIntent: String, CaseIterable, Identifiable {
    case existing, importProfile, create
    var id: String { rawValue }
    var title: String { switch self { case .existing: return "已有服务器"; case .importProfile: return "导入配置"; case .create: return "新建服务器" } }
}
enum AuthorizationBrowser: String, CaseIterable, Identifiable {
    case egoLite, system
    var id: String { rawValue }
    var title: String { self == .egoLite ? "Ego Lite" : "系统默认浏览器" }
    static var egoURL: URL? { NSWorkspace.shared.urlForApplication(withBundleIdentifier: "com.citrolabs.ego.lite") }
}
struct ExistingServer: Decodable, Equatable, Identifiable {
    let id: Int64
    let name: String
    let status: String
    let region: String
    let memory: Int
    let disk: Int
    let monthly: Double
    let canRead: Bool
    var detail: String { "\(region.uppercased()) · \(memory) MB · \(disk) GB" }
    var price: String { monthly > 0 ? String(format: "原套餐 US$%.2f / 月", monthly) : "沿用当前套餐" }
}

@MainActor final class ConnectionSetupModel: ObservableObject {
    @Published private(set) var intent: SetupIntent
    @Published private(set) var authorized = false
    @Published private(set) var canCreatePermission = false
    @Published var working = false
    @Published private(set) var ready = false
    @Published private(set) var accountLocked = false
    @Published private(set) var hasDeployment = false
    @Published private(set) var servers: [ExistingServer] = []
    @Published private(set) var selectedServerID: Int64?
    @Published private(set) var plan: CloudPlanViewData?
    @Published var consent = false
    @Published private(set) var message = "连接自己的 DigitalOcean，优先沿用已有服务器和套餐。"
    @Published var error: String?
    @Published private(set) var profile: Profile?
    @Published private(set) var saved = false
    @Published private(set) var authorizationPending = false
    @Published private(set) var authorizationExpires: Date?
    @Published private(set) var pairingCommand: String?
    @Published private(set) var pairingExpires: Date?
    @Published var browser: AuthorizationBrowser {
        didSet { persistBrowser(browser) }
    }
    let browsers: [AuthorizationBrowser]
    private var authorizationURL: URL?
    private var consoleURL: URL?
    private var child: JSONChild?
    private var loadTask: Task<Void, Never>?
    private var closed = false
    private var loginAfterReady = false
    private let saveState: (Data) throws -> Void
    private let loadState: () throws -> Data?
    private let unlockState: () throws -> Data?
    private var allowKeychainInteraction = false
    private let openOverride: ((URL) -> Bool)?
    private let status: AssistantStatusStore
    private let persistBrowser: (AuthorizationBrowser) -> Void
    private let writeClipboard: (String) -> Void
    private let readClipboard: () -> String?
    var sendOverride: (([String: Any]) throws -> Void)?

    init(intent: SetupIntent = .existing, load: @escaping () throws -> Data? = { try CloudVault.load() },
         unlock: @escaping () throws -> Data? = { try CloudVault.load(interactionAllowed: true) },
         save: @escaping (Data) throws -> Void = CloudVault.save,
         open: ((URL) -> Bool)? = nil, writeStatus: Bool = true, status: AssistantStatusStore? = nil,
         persistBrowser: @escaping (AuthorizationBrowser) -> Void = { UserDefaults.standard.set($0.rawValue, forKey: "authorizationBrowser") },
         writeClipboard: @escaping (String) -> Void = { NSPasteboard.general.clearContents(); NSPasteboard.general.setString($0, forType: .string) },
         readClipboard: @escaping () -> String? = { NSPasteboard.general.string(forType: .string) }) {
        self.intent = intent; loadState = load; unlockState = unlock; saveState = save; openOverride = open
        self.status = status ?? (writeStatus ? .shared : AssistantStatusStore())
        self.persistBrowser = persistBrowser; self.writeClipboard = writeClipboard; self.readClipboard = readClipboard
        let available: [AuthorizationBrowser] = AuthorizationBrowser.egoURL == nil ? [.system] : [.egoLite, .system]
        browsers = available
        let stored = UserDefaults.standard.string(forKey: "authorizationBrowser").flatMap(AuthorizationBrowser.init(rawValue:))
        browser = stored.flatMap { available.contains($0) ? $0 : nil } ?? available[0]
    }
    var selectedServer: ExistingServer? { servers.first { $0.id == selectedServerID } }
    var canDeploy: Bool { intent == .create && authorized && canCreatePermission && !working && consent && !hasDeployment && plan != nil && (plan?.expires ?? 0) > Int64(Date().timeIntervalSince1970) }
    var canPair: Bool { intent == .existing && authorized && !working && selectedServer?.canRead == true }
    var canRestartAuthorization: Bool { authorizationPending || !working }
    var authorizationPageReady: Bool { authorizationURL != nil }

    static func validAuthorizationURL(_ raw: String) -> URL? {
        guard let url = URL(string: raw), url.scheme == "https", url.host == "cloud.digitalocean.com", url.path == "/v1/oauth/authorize", url.user == nil, url.password == nil, url.port == nil else { return nil }
        return url
    }
    func start() {
        guard intent != .importProfile, child == nil, loadTask == nil, !closed, sendOverride == nil else { return }
        working = true; message = "正在读取本机保存的账号…"; writeStatus()
        let loader = allowKeychainInteraction ? unlockState : loadState
        loadTask = Task {
            do {
                let data = try await Task.detached { try loader() }.value
                guard !closed, !Task.isCancelled else { return }
                var request: [String: Any] = ["intent": intent == .create ? "create" : "existing"]
                if let data { request["state"] = try JSONSerialization.jsonObject(with: data) }
                let process = JSONChild(executable: CoreRPC.coreURL, arguments: ["cloud"]); child = process
                process.onEvent = { [weak self] in self?.accept($0) }
                process.onExit = { [weak self] _ in
                    guard let self, !self.closed else { return }
                    self.child = nil; self.working = false; self.ready = false; self.authorizationPending = false
                    self.error = "账号进程已停止，请重新打开连接设置。"; self.writeStatus()
                }
                try process.start(); try process.send(request)
            } catch {
                working = false
                let code = error as NSError
                accountLocked = code.domain == NSCocoaErrorDomain && code.code == CocoaError.fileReadNoPermission.rawValue
                self.error = accountLocked ? nil : "无法读取本机账号配置，可重试读取或改用配置导入。"
                if accountLocked { message = "账号保存在钥匙串中，解锁后即可读取已有服务器。" }
            }
            loadTask = nil; writeStatus()
        }
    }
    func unlockAccount() {
        guard !working else { return }
        allowKeychainInteraction = true; accountLocked = false; error = nil; start()
    }
    private func send(_ request: [String: Any]) throws {
        if let sendOverride { try sendOverride(request) }
        else if let child { try child.send(request) }
        else { throw CocoaError(.executableNotLoadable) }
    }
    func selectIntent(_ value: SetupIntent) {
        guard !working, value != intent else { return }
        intent = value; consent = false; plan = nil; error = nil; saved = false; profile = nil
        pairingCommand = nil; pairingExpires = nil; consoleURL = nil
        if value == .existing { message = "读取已有服务器，沿用原套餐。"; if authorized { command("servers") } }
        if value == .create { message = "新建会新增收费服务器；只有确认方案和费用后才会创建。"; if authorized && canCreatePermission { command("plan") } }
        if value == .importProfile { message = "导入你已有的连接或加密迁移文件。" }
        if value != .importProfile && child == nil && !accountLocked { start() }
        writeStatus()
    }
    func selectServer(_ id: Int64?) {
        guard !working else { return }
        selectedServerID = id; pairingCommand = nil; pairingExpires = nil; consoleURL = nil; profile = nil; saved = false; error = nil; writeStatus()
    }
    func command(_ action: String, code: String? = nil) {
        guard !working else { return }
        if action == "login", child == nil, sendOverride == nil { loginAfterReady = true; start(); return }
        if action == "deploy" && !canDeploy { return }
        if action == "plan" && (intent != .create || !canCreatePermission) { return }
        if action == "pairExisting" && !canPair { return }
        error = nil; working = true
        if action == "login" { authorizationPending = true; authorizationURL = nil; authorizationExpires = nil; pairingCommand = nil; profile = nil; saved = false; message = "正在准备 DigitalOcean 应用授权…" }
        if action == "servers" { message = "正在读取已有服务器…" }
        if action == "plan" { plan = nil; consent = false; message = "正在读取新建方案与价格…" }
        if action == "pairExisting" { pairingCommand = nil; message = "正在读取所选服务器的现有连接…" }
        if action == "resume" { profile = nil; saved = false }
        do {
            var request: [String: Any] = ["action": action, "intent": intent == .create ? "create" : "existing"]
            if action == "deploy" { request["id"] = plan?.id }
            if action == "pairExisting" { request["serverID"] = selectedServerID }
            if action == "acceptPairing" { request["code"] = code ?? "" }
            try send(request)
        } catch { working = false; authorizationPending = false; self.error = "无法发起操作，请重新打开连接设置。" }
        writeStatus()
    }
    func restartAuthorization() {
        guard canRestartAuthorization else { return }
        if sendOverride != nil { working = false; command("login"); return }
        child?.onExit = nil; child?.stop(); child = nil; loadTask?.cancel(); loadTask = nil
        authorizationPending = false; authorizationURL = nil; authorizationExpires = nil; working = false; ready = false; loginAfterReady = true
        start()
    }
    func reopenAuthorization() {
        guard let url = authorizationURL, (authorizationExpires ?? .distantPast) > Date() else { error = "本次授权已过期，请重新授权。"; return }
        open(url)
    }
    private func open(_ url: URL) {
        if let openOverride { if !openOverride(url) { error = "无法打开授权页面，请选择其他浏览器后重试。" }; return }
        guard browser == .egoLite else { if !NSWorkspace.shared.open(url) { error = "无法打开授权页面。" }; return }
        guard let app = AuthorizationBrowser.egoURL else { error = "未找到 Ego Lite，请选择系统默认浏览器。"; return }
        NSWorkspace.shared.open([url], withApplicationAt: app, configuration: .init()) { [weak self] _, error in
            if error != nil { Task { @MainActor in self?.error = "Ego Lite 未能打开授权页，请选择其他浏览器重试。" } }
        }
    }
    func copyPairingAndOpenConsole() {
        guard let command = pairingCommand, let url = consoleURL, (pairingExpires ?? .distantPast) > Date() else { error = "配对已过期，请重新读取连接。"; return }
        writeClipboard(command)
        open(url)
    }
    func pastePairingCode() { command("acceptPairing", code: readClipboard() ?? "") }
    func accept(_ event: [String: Any]) {
        switch event["event"] as? String {
        case "cloudState":
            do {
                guard let state = event["state"], let id = event["id"] as? String else { throw CocoaError(.fileReadCorruptFile) }
                try saveState(JSONSerialization.data(withJSONObject: state)); try send(["action": "stored", "id": id])
            } catch { child?.stop(); working = false; self.error = "账号未能保存到钥匙串，后续云操作已停止。" }
        case "cloudReady":
            ready = true; accountLocked = false; working = false; authorized = event["authorized"] as? Bool == true; hasDeployment = event["hasDeployment"] as? Bool == true; canCreatePermission = event["canCreate"] as? Bool == true
            if loginAfterReady { loginAfterReady = false; command("login") }
            else if authorized && intent == .existing { command("servers") }
            else if authorized && intent == .create && canCreatePermission { command("plan") }
            else if authorized && intent == .create { message = "新建需要单独授权创建权限，确认报价前不会创建资源。" }
            else { message = "登录自己的 DigitalOcean，读取已有服务器。" }
        case "cloudAuthorize":
            guard let raw = event["url"] as? String, let url = Self.validAuthorizationURL(raw) else { working = false; authorizationPending = false; error = "授权地址无效，已停止。"; break }
            authorizationURL = url; authorizationPending = true
            message = "授权页已在所选浏览器打开，等待确认。"
            authorizationExpires = Date(timeIntervalSince1970: (event["expires"] as? NSNumber)?.doubleValue ?? Date().addingTimeInterval(300).timeIntervalSince1970)
            open(url)
        case "cloudAuthorized":
            authorized = true; authorizationPending = false; authorizationURL = nil; authorizationExpires = nil; canCreatePermission = event["canCreate"] as? Bool == true
            message = intent == .create ? "账号授权完成，正在读取新建方案…" : "账号授权完成，正在读取已有服务器…"
        case "cloudServers":
            if let raw = event["servers"], let data = try? JSONSerialization.data(withJSONObject: raw), let list = try? JSONDecoder().decode([ExistingServer].self, from: data) {
                servers = list; if !list.contains(where: { $0.id == selectedServerID }) { selectedServerID = list.count == 1 ? list.first?.id : nil }
                working = false; message = list.isEmpty ? "账号中没有找到服务器。可导入已有配置，或另行选择新建。" : "选择已有服务器，沿用它当前的套餐和服务。"
            } else { working = false; error = "服务器列表无效，请重试。" }
        case "cloudPairing":
            let expected = selectedServerID.map { "/droplets/\($0)/terminal/ui" }
            guard let raw = event["consoleURL"] as? String, let url = URL(string: raw), url.scheme == "https", url.host == "cloud.digitalocean.com", url.path.trimmingCharacters(in: CharacterSet(charactersIn: "/")) == expected?.trimmingCharacters(in: CharacterSet(charactersIn: "/")), url.query == "os_user=root", url.user == nil, url.password == nil, url.port == nil, let command = event["command"] as? String, command.utf8.count < 64_000 else { working = false; error = "控制台配对信息无效，请重新读取。"; break }
            pairingCommand = command; consoleURL = url; pairingExpires = Date(timeIntervalSince1970: (event["expires"] as? NSNumber)?.doubleValue ?? 0)
            working = false; message = "需要通过官方控制台确认读取这台服务器的连接，完成一次配对。"
        case "cloudExistingProfile", "cloudDeployed":
            guard let raw = event["profile"], let data = try? JSONSerialization.data(withJSONObject: raw), let value = try? JSONDecoder().decode(Profile.self, from: data) else { working = false; error = "连接数据无效，尚未保存。"; break }
            if event["event"] as? String == "cloudDeployed" { hasDeployment = true }
            profile = value; working = true; pairingCommand = nil; message = "正在将连接保存到本机钥匙串…"
        case "cloudPlan":
            guard intent == .create else { working = false; break }
            if let raw = event["plan"], let data = try? JSONSerialization.data(withJSONObject: raw), let value = try? JSONDecoder().decode(CloudPlanViewData.self, from: data), value.monthly > 0, value.hourly > 0 {
                plan = value; consent = false; working = false; message = "这是新增服务器的报价。确认费用后才会创建。"
            } else { working = false; error = "新建报价无效，请重新获取。" }
        case "cloudExisting": hasDeployment = true; working = false; message = "上次新建有部署记录，可继续检查；不会重复创建。"
        case "cloudProgress": message = event["message"] as? String ?? "正在处理…"
        case "cloudError", "error":
            working = false; authorizationPending = false; consent = false; error = event["message"] as? String ?? "操作未完成，请重试。"
            hasDeployment = event["hasDeployment"] as? Bool ?? hasDeployment
            if event["code"] as? String == "authorization" { authorized = false; canCreatePermission = false }
        default: break
        }
        writeStatus()
    }
    func close() { closed = true; loadTask?.cancel(); child?.stop(); child = nil; working = false; authorizationURL = nil; writeStatus() }
    func markSaved() { saved = true; working = false; error = nil; message = "连接已保存，可继续检查网络并验证出口。"; writeStatus() }
    func profileSaveFailed(_ cause: Error? = nil) {
        working = false
        let detail = cause.map { $0 as NSError }
        if detail?.domain == "Clearline" { error = detail?.localizedDescription }
        else if detail?.domain == NSCocoaErrorDomain && detail?.code == CocoaError.fileWriteNoPermission.rawValue { error = "连接尚未保存，请允许系统钥匙串访问后重新保存。" }
        else { error = "连接尚未保存，请确认配置有效后重试。" }
        writeStatus()
    }
    func copyAssistantInstructions() {
        let text = """
        按照 https://github.com/wenh09/clearline 的 AGENTS.md 协助我使用 Clearline。优先沿用我自己的 DigitalOcean 已有服务器和套餐。
        先查看客户端主窗口及 setup-status.json 的 connection 段；账号授权、已有连接、本机代理和出口验证是不同阶段。不要从 hasDeployment 或旧报价推断需要新建服务器。
        登录、OAuth 和系统安全提示由我完成。使用应用的已有服务器读取/加密配对或配置导入，勿读取钥匙串令牌、私钥或记录连接密码。
        检查其他代理对服务器入口的影响，保留可用联网方式；以实际出口验证确认成功。只有我另行选择新建并确认费用，才能创建收费资源。
        """
        writeClipboard(text)
    }
    private func writeStatus() {
        let phase = closed ? "closed" : saved ? "saved" : intent == .importProfile ? "import" : accountLocked ? "account_locked" : authorizationPending ? "awaiting_authorization" : pairingCommand != nil ? "awaiting_pairing" : authorized ? "server_selection" : "account"
        status.updateCloud(.init(active: !closed, intent: intent.rawValue, authorized: ready ? authorized : nil, working: working, phase: phase, existingServerCount: servers.count, serverSelected: selectedServerID != nil, hasAppDeployment: hasDeployment, needsAttention: error != nil || (accountLocked && intent != .importProfile), quotedMonthlyUSD: intent == .create && !closed ? plan?.monthly : nil))
    }
}
