import AppKit
import Foundation
import Network
import ClearlineKit
import Combine

@MainActor final class AppModel: ObservableObject {
    enum State: String { case idle = "准备连接", connecting = "正在连接", connected = "已连接", reconnecting = "正在恢复连接", disconnecting = "正在断开", failed = "连接未完成" }
    @Published var profiles: [Profile] = []
    enum RoutingMode: String, CaseIterable, Identifiable {
        case ai, all
        var id: String { rawValue }
        var title: String { self == .ai ? "AI 分流" : "普通代理" }
        var help: String { self == .ai ? "AI 网站经美国服务器；DNS、UDP 和 IPv6 统一经服务器，其他 IPv4 网页直连。" : "浏览器等遵循系统代理的连接经服务器；DNS 和 UDP 沿用系统设置。" }
        var coreMode: String { self == .ai ? "ai" : "all" }
        static func restoring(_ stored: String?) -> Self { stored == "global" ? .ai : Self(rawValue: stored ?? "") ?? .ai }
    }
    @Published private(set) var routingMode = RoutingMode.restoring(UserDefaults.standard.string(forKey: "routingMode"))
    var routingCaption: String { routingMode == .ai ? "AI 防泄漏 · 美国" : "普通代理 · 美国" }
    @Published private(set) var fallbackNotice: String?
    var usesPacketTunnel: Bool { routingMode == .ai }
    var protectionNote: String { packetTunnelActive ? "AI 防泄漏 · DNS / UDP 已接管" : "普通代理 · DNS / UDP 沿用系统设置" }
    @Published var needsTunnelApproval = false
    @Published private(set) var packetTunnelActive = false
    @Published private(set) var systemProxyActive = false
    private let proxyInspector: (Int) -> Bool
    private var proxyOwnershipTask: Task<Void, Never>?
    var proxyOwnershipNote: String? { connected && !usesPacketTunnel && !systemProxyActive ? "服务器通道已连接；其他 VPN 或代理仍在接管系统流量。" : nil }
    @Published var showTunnelDiagnostic = false
    @Published private(set) var diagnosticRunning = false
    @Published private(set) var diagnosticStage = "idle"
    @Published private(set) var diagnosticMessage = "仅测试临时网卡、隧道 DNS 和 UDP，不修改系统 DNS 或默认路由。"
    @Published private(set) var diagnosticPassed: Bool?
    private var diagnosticChild: JSONChild?
    private var diagnosticPending = false
    private static let diagnosticStages = ["transport": "验证原服务器", "udp_relay": "验证 UDP 出口", "utun": "创建临时网卡", "utun_dns_udp": "验证 UDP DNS", "utun_dns_tcp": "验证 TCP DNS", "utun6_dns_udp": "验证 IPv6 UDP DNS", "utun6_dns_tcp": "验证 IPv6 TCP DNS"]
    var diagnosticStageTitle: String { Self.diagnosticStages[diagnosticStage] ?? "系统隧道诊断" }
    func startTunnelDiagnostic() {
        guard !busy, !diagnosticRunning, !packetTunnelActive, let profile = selected else { return }
        guard !connected || profile.kind == "hysteria2" else { diagnosticMessage = "请先断开这条连接再检测，避免同一 WireGuard 身份建立并行会话。"; return }
        do { guard try preparePacketTunnel() else { diagnosticPending = true; needsTunnelApproval = true; return } }
        catch { diagnosticPending = true; needsTunnelApproval = true; return }
        diagnosticPassed = nil; diagnosticRunning = true; diagnosticStage = "transport"; diagnosticMessage = "正在验证原服务器…"
        let child = JSONChild(executable: coreExecutable, arguments: ["diagnoseTunnel"]); diagnosticChild = child
        child.onEvent = { [weak self, weak child] event in
            guard let self, let child, self.diagnosticChild === child, self.diagnosticRunning else { return }
            switch event["event"] as? String {
            case "diagnosticProgress":
                if let stage = event["stage"] as? String, Self.diagnosticStages[stage] != nil { self.diagnosticStage = stage; self.diagnosticMessage = self.diagnosticStageTitle }
            case "tunnelDiagnostic": self.diagnosticPassed = event["ok"] as? Bool == true; self.diagnosticRunning = false; self.diagnosticMessage = event["message"] as? String ?? "检测结束"
            case "error": self.diagnosticPassed = false; self.diagnosticRunning = false; self.diagnosticMessage = event["message"] as? String ?? "系统隧道诊断失败"
            default: break
            }
        }
        child.onExit = { [weak self, weak child] _ in
            guard let self, let child, self.diagnosticChild === child else { return }
            if self.diagnosticRunning { self.diagnosticRunning = false; self.diagnosticPassed = false; self.diagnosticMessage = "检测进程已停止；现有代理连接保持不变。" }
            self.diagnosticChild = nil
        }
        do { try child.start(); try child.send(["profile": CoreRPC.dictionary(profile)]); child.closeInput() }
        catch { diagnosticRunning = false; diagnosticPassed = false; diagnosticMessage = "无法启动诊断"; child.stop(); diagnosticChild = nil }
    }
    func stopTunnelDiagnostic() {
        diagnosticRunning = false; diagnosticChild?.stop(); diagnosticChild = nil
        diagnosticMessage = "检测已取消"; diagnosticStage = "cancelled"
    }
    private let preparePacketTunnel: () throws -> Bool
    private func prepareAITunnel() -> Bool {
        guard usesPacketTunnel else { return true }
        do {
            guard try preparePacketTunnel() else { needsTunnelApproval = true; return false }
            return true
        } catch { needsTunnelApproval = true; message = "请先通过 macOS 安装器安装本版本的系统隧道组件。"; return false }
    }
    func finishTunnelInstallation() {
        needsTunnelApproval = false
        if diagnosticPending { diagnosticPending = false; startTunnelDiagnostic() }
        else {
            if connected { setRoutingMode(.ai, force: true) } else { routingMode = .ai; UserDefaults.standard.set("ai", forKey: "routingMode"); connect() }
        }
    }

    func setRoutingMode(_ mode: RoutingMode, force: Bool = false) {
        guard (mode != routingMode || force), !busy, !loadingProfiles, !diagnosticRunning else { return }
        let previous = routingMode
        fallbackNotice = nil
        routingMode = mode; UserDefaults.standard.set(mode.rawValue, forKey: "routingMode")
        if connected && !prepareAITunnel() { routingMode = previous; UserDefaults.standard.set(previous.rawValue, forKey: "routingMode"); return }
        guard connected, let profile = selected else { return }
        // Retire callbacks from the old session before restoring its proxy.
        // If restoration fails, retain both the live tunnel and its old scope.
        wantsConnection = true; backoff.reset()
        retryTask?.cancel(); connectTimeout?.cancel(); session = UUID()
        state = .disconnecting
        let token = session
        startProgress(.switching, phase: .restoring)
        Task {
            let restored = await clean()
            guard session == token, wantsConnection else { return }
            guard restored else {
                routingMode = previous; UserDefaults.standard.set(previous.rawValue, forKey: "routingMode")
                finishProgress(); statusNote = message; state = .connected
                return
            }
            if usesPacketTunnel {
                let shadowrocket = NSWorkspace.shared.runningApplications.contains { $0.bundleIdentifier == "com.liguangming.Shadowrocket" }
                let report = await preflightInspector(profile, nil, shadowrocket)
                guard session == token, wantsConnection else { return }
                if report.requiresAcknowledgement {
                    routingMode = .all; UserDefaults.standard.set("all", forKey: "routingMode")
                    fallbackNotice = "其他 VPN 或代理仍在接管网络，已恢复普通代理；AI 防泄漏保护未启用。"
                    begin(profile); return
                }
            }
            begin(profile)
        }
    }
    static let defaultDomains = (try? String(contentsOf: Bundle.main.bundleURL.appendingPathComponent("Contents/Resources/ai-domains.txt"), encoding: .utf8)) ?? "claude.ai\nclaude.com\nanthropic.com\nclaudeusercontent.com\nclaudemcpcontent.com\nchatgpt.com\nopenai.com\noaistatic.com\noaiusercontent.com\noaistatsig.com\ncdn.openaimerge.com\nchallenges.cloudflare.com\ncdn.workos.com\nforwarder.workos.com\nfrontend-apps-multi-region.workos.com\nimages.workoscdn.com\nsetup.workos.com\nworkos.imgix.net\nwww.cloudflare.com\none.one.one.one\ncloudflare-dns.com\ndns.google\ndns.quad9.net\ndoh.opendns.com\ndns.alidns.com\ndoh.pub"
    private let reconnect = true
    @Published var state: State = .idle
    @Published var message = ""
    @Published var latency: Int?
    @Published var connectMs: Int?
    @Published var httpPort: Int?
    @Published var upload: Int64 = 0
    @Published var download: Int64 = 0
    @Published var connectedAt: Date?
    struct ExitVerification {
        let ip: String
        let country: String
        let verifiedAt: Date
        var region: String { Locale(identifier: "zh_CN").localizedString(forRegionCode: country) ?? country }
    }
    @Published private(set) var exitVerification: ExitVerification?
    @Published private(set) var isVerifyingExit = false
    @Published private(set) var exitError: String?
    private var exitRequestID: String?
    private var exitTimeout: Task<Void, Never>?

    func verifyExit() {
        guard connected, let core, !isVerifyingExit, !isSpeedTesting else { return }
        clearExitVerification()
        let id = UUID().uuidString; exitRequestID = id; isVerifyingExit = true
        do {
            try core.send(["action": "verifyExit", "id": id])
            exitTimeout = Task {
                try? await Task.sleep(nanoseconds: 15_000_000_000)
                guard !Task.isCancelled, exitRequestID == id, isVerifyingExit else { return }
                isVerifyingExit = false; exitRequestID = nil; exitError = "验证超时，请重试"
            }
        } catch {
            isVerifyingExit = false; exitRequestID = nil; exitError = "无法发起验证，请重试"
        }
    }
    private func clearExitVerification() {
        exitTimeout?.cancel(); exitTimeout = nil; exitRequestID = nil
        isVerifyingExit = false; exitVerification = nil; exitError = nil
    }
    private func receiveExitVerification(_ event: [String: Any]) {
        guard isVerifyingExit, event["id"] as? String == exitRequestID else { return }
        exitTimeout?.cancel(); exitTimeout = nil; isVerifyingExit = false; exitRequestID = nil
        guard event["ok"] as? Bool == true,
              let ip = event["ip"] as? String, let country = event["country"] as? String,
              let timestamp = event["verifiedAt"] as? String,
              let date = ISO8601DateFormatter().date(from: timestamp) else {
            exitError = event["message"] as? String ?? "未获得有效结果，请重试"
            return
        }
        exitVerification = ExitVerification(ip: ip, country: country, verifiedAt: date)
    }
    @Published var connectionProgress: ConnectionProgress?
    @Published var retryUntil: TimeInterval?
    @Published private(set) var proxyRetainedDuringRecovery = false
    @Published var lastHealthAt: Date?
    @Published private(set) var statusNote: String?
    private var operationNow: TimeInterval { ProcessInfo.processInfo.systemUptime }
    private func startProgress(_ kind: ConnectionProgress.Kind, phase: ConnectionProgress.Phase) {
        statusNote = nil
        connectionProgress = ConnectionProgress(kind: kind, phase: phase, now: operationNow)
    }
    private func advanceProgress(_ phase: ConnectionProgress.Phase) {
        guard var progress = connectionProgress else { return }
        progress.transition(to: phase, now: operationNow); connectionProgress = progress
    }
    private func finishProgress() {
        guard var progress = connectionProgress else { return }
        progress.finish(now: operationNow); connectionProgress = progress
    }
    @Published private(set) var speedReport: NetworkSpeedReport?
    @Published private(set) var isSpeedTesting = false
    @Published private(set) var isCancellingSpeed = false
    @Published private(set) var speedStage = ""
    @Published private(set) var speedBytes = 0
    @Published private(set) var speedError: String?
    private var directSpeedCore: JSONChild?
    private var speedRequestID: String?
    private var speedTimeout: Task<Void, Never>?

    func startSpeedTest() {
        guard !busy, !diagnosticRunning, !isSpeedTesting, !isVerifyingExit else { return }
        clearSpeedTest()
        let id = UUID().uuidString; speedRequestID = id; isSpeedTesting = true; speedStage = "Claude 连通性"
        do {
            if connected, let core {
                try core.send(["action": "speedTest", "id": id])
            } else {
                let child = JSONChild(executable: CoreRPC.coreURL, arguments: ["speedDirect"])
                directSpeedCore = child
                child.onEvent = { [weak self] event in self?.receiveSpeedEvent(event) }
                child.onExit = { [weak self] _ in
                    guard let self, self.speedRequestID == id, self.isSpeedTesting else { return }
                    self.speedTimeout?.cancel(); self.speedTimeout = nil; self.directSpeedCore = nil
                    self.speedRequestID = nil; self.isSpeedTesting = false; self.isCancellingSpeed = false
                    self.speedError = "测速进程已停止，请重试"
                }
                try child.start(); try child.send(["id": id])
            }
            speedTimeout = Task {
                try? await Task.sleep(nanoseconds: 45_000_000_000)
                guard !Task.isCancelled, speedRequestID == id, isSpeedTesting else { return }
                try? (directSpeedCore ?? core)?.send(["action": "cancelSpeedTest", "id": id])
                speedRequestID = nil; isSpeedTesting = false; isCancellingSpeed = false
                speedError = "测速超时，请重试"; speedStage = ""
                directSpeedCore?.stop(); directSpeedCore = nil
            }
        } catch {
            speedRequestID = nil; isSpeedTesting = false; speedError = "无法开始测速"
            directSpeedCore?.stop(); directSpeedCore = nil
        }
    }
    func stopSpeedTest() {
        guard let id = speedRequestID, isSpeedTesting, !isCancellingSpeed else { return }
        isCancellingSpeed = true; speedStage = "正在停止测速"
        do { try (directSpeedCore ?? core)?.send(["action": "cancelSpeedTest", "id": id]) }
        catch { isSpeedTesting = false; isCancellingSpeed = false; speedRequestID = nil; speedError = "测速已停止" }
    }
    private func clearSpeedTest() {
        if let id = speedRequestID { try? (directSpeedCore ?? core)?.send(["action": "cancelSpeedTest", "id": id]) }
        directSpeedCore?.stop(); directSpeedCore = nil
        speedTimeout?.cancel(); speedTimeout = nil; speedRequestID = nil
        isSpeedTesting = false; isCancellingSpeed = false; speedReport = nil
        speedError = nil; speedStage = ""; speedBytes = 0
    }
    private func receiveSpeedEvent(_ event: [String: Any]) {
        guard isSpeedTesting, event["id"] as? String == speedRequestID else { return }
        if let value = event["report"], let data = try? JSONSerialization.data(withJSONObject: value),
           let report = try? JSONDecoder().decode(NetworkSpeedReport.self, from: data) { speedReport = report }
        if event["event"] as? String == "speedProgress" {
            guard !isCancellingSpeed else { return }
            switch event["stage"] as? String {
            case "claude": speedStage = "正在检测 Claude"
            case "ordinary": speedStage = "正在检测普通网络"
            case "download": speedStage = "正在下载采样"; speedBytes = event["bytes"] as? Int ?? 0
            default: break
            }
        } else {
            speedTimeout?.cancel(); speedTimeout = nil; speedRequestID = nil
            directSpeedCore?.closeInput(); directSpeedCore = nil
            let cancelled = isCancellingSpeed
            isSpeedTesting = false; isCancellingSpeed = false
            speedStage = cancelled ? "已停止测速" : ""
            if !cancelled && event["ok"] as? Bool != true { speedError = event["message"] as? String ?? "测速失败，请重试" }
        }
    }
    @Published var activity: [String] = []
    @Published var showImport = false
    @Published var showCloudSetup = false
    @Published var showNetworkCheck = false
    @Published private(set) var checkingPreflight = false
    @Published private(set) var networkPreflight: NetworkPreflight?
    var connectAfterSetup = false
    var verifyAfterSetup = false
    var importAfterSetup = false
    @Published var showTransfer = false
    @Published var transferImport = false
    @Published var transferFile: URL?
    @Published var importText = ""
    @Published var importName = "我的服务器"
    @Published var storageError = false
    @Published var loadingProfiles = false
    private var core: JSONChild?
    private var agent: JSONChild?
    private var session = UUID()
    private var wantsConnection = false
    private var backoff = ReconnectBackoff()
    private var lastFailureRetryable = true
    private var preferredProfileID: String?
    private var waitingForNetwork = false
    private var coreSupportsRetention = false
    private var transportReady = false
    private var proxyApplied = false
    private var networkCheckTask: Task<Void, Never>?
    private var networkSignature: String?
    private var wakeObserver: NSObjectProtocol?
    private var lastCheckAt: TimeInterval = 0
    private let connectionCheckPause: UInt64
    private var cleanupTask: Task<Bool, Never>?
    private let configurationLoader: (Bool) throws -> [Profile]
    private let coreExecutable: URL
    private let agentExecutable: URL
    private let recoveryFile: URL
    private let reconnectPause: (Int) -> TimeInterval
    private var retryTask: Task<Void, Never>?
    private var connectTimeout: Task<Void, Never>?
    private let pathMonitor = NWPathMonitor()
    private var lastPath: NWPath.Status?
    private var activationObserver: NSObjectProtocol?
    let assistantStatus: AssistantStatusStore
    private var statusObserver: AnyCancellable?
    private var statusUpdatePending = false
    private let preflightInspector: (Profile, Int?, Bool) async -> NetworkPreflight
    private let profileSaver: ([Profile]) throws -> Void
    private let profileValidator: (Profile) async throws -> Void
    private var preflightTask: Task<Void, Never>?
    private var preflightID: UUID?

    var selected: Profile? { profiles.first { $0.kind == "hysteria2" } ?? profiles.first }
    var statusTitle: String {
        if checkingPreflight { return "检查本机网络" }
        if loadingProfiles { return "读取配置中" }
        if storageError { return "需要解锁配置" }
        if profiles.isEmpty { return "设置连接" }
        return state == .idle ? "未连接" : state.rawValue
    }
    var primaryActionTitle: String {
        if connected || busy { return "断开连接" }
        if storageError { return "解锁配置" }
        if profiles.isEmpty { return "设置连接" }
        return "连接"
    }
    var actionMessage: String? {
        if loadingProfiles { return nil }
        if storageError { return "点击按钮，在系统弹窗中确认。" }
        if let note = statusNote { return note }
        if state == .failed || (recoveryNeeded && !busy) { return message.isEmpty ? "请从“工具”菜单恢复网络。" : message }
        return nil
    }
    func openTransfer(importing: Bool) {
        guard !storageError else { reloadProfiles(interactionAllowed: true); return }
        transferImport = importing; showTransfer = true
    }
    func performPrimaryAction() {
        if !storageError && profiles.isEmpty && !connected && !busy { showCloudSetup = true }
        else { toggle() }
    }
    func copyTerminalProxy() {
        guard let port = httpPort else { return }
        NSPasteboard.general.clearContents()
        NSPasteboard.general.setString("export HTTPS_PROXY=http://127.0.0.1:\(port) HTTP_PROXY=http://127.0.0.1:\(port)", forType: .string)
    }
    var busy: Bool { checkingPreflight || [.connecting, .reconnecting, .disconnecting].contains(state) }
    var connected: Bool { state == .connected }
    var backupURL: URL { recoveryFile }
    private let tunnelRecoveryPending: () -> Bool
    private var hasTunnelRecoveryRecord: Bool { tunnelRecoveryPending() }
    var recoveryNeeded: Bool { (FileManager.default.fileExists(atPath: backupURL.path) || hasTunnelRecoveryRecord) && !connected }

    init(configurationLoader: @escaping (Bool) throws -> [Profile] = { try KeychainStore.load(interactionAllowed: $0) },
         coreExecutable: URL = CoreRPC.coreURL,
         agentExecutable: URL = Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/ClearlineNetworkAgent"),
         recoveryFile: URL = FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("Clearline/network-restore.plist"),
         monitorSystemEvents: Bool = true,
         reconnectPause: @escaping (Int) -> TimeInterval = { Double($0) },
         connectionCheckPause: UInt64 = 500_000_000,
         assistantStatus: AssistantStatusStore? = nil,
         preflight: @escaping (Profile, Int?, Bool) async -> NetworkPreflight = NetworkPreflight.inspect,
         saveProfiles: @escaping ([Profile]) throws -> Void = KeychainStore.save,
         validateProfile: ((Profile) async throws -> Void)? = nil,
         preparePacketTunnel: @escaping () throws -> Bool = TunnelService.prepare,
         proxyInspector: @escaping (Int) -> Bool = ProxySnapshot.ownsSystemProxy,
         tunnelRecoveryPending: @escaping () -> Bool = { [TunnelNetwork.journalPath, TunnelNetwork.legacyJournalPath].contains { FileManager.default.fileExists(atPath: $0) } }) {
        self.tunnelRecoveryPending = tunnelRecoveryPending
        self.proxyInspector = proxyInspector
        self.preparePacketTunnel = preparePacketTunnel
        self.assistantStatus = assistantStatus ?? .shared
        self.preflightInspector = preflight; self.profileSaver = saveProfiles
        self.profileValidator = validateProfile ?? { profile in _ = try await CoreRPC.call("validate", ["profile": CoreRPC.dictionary(profile)]) }
        self.configurationLoader = configurationLoader; self.coreExecutable = coreExecutable; self.agentExecutable = agentExecutable
        self.recoveryFile = recoveryFile; self.reconnectPause = reconnectPause; self.connectionCheckPause = connectionCheckPause
        if ["claude", "global"].contains(UserDefaults.standard.string(forKey: "routingMode") ?? "") { UserDefaults.standard.set("ai", forKey: "routingMode") }
        reloadProfiles()
        statusObserver = objectWillChange.sink { [weak self] _ in
            guard let self, !self.statusUpdatePending else { return }
            self.statusUpdatePending = true
            DispatchQueue.main.async { [weak self] in self?.statusUpdatePending = false; self?.writeAssistantStatus() }
        }
        writeAssistantStatus()
        guard monitorSystemEvents else { return }
        pathMonitor.pathUpdateHandler = { [weak self] path in
            let signature = NetworkPathSignature.capture(path)
            Task { @MainActor in self?.networkChanged(path.status, signature: signature) }
        }
        pathMonitor.start(queue: DispatchQueue(label: "org.clearline.client.path"))
        wakeObserver = NSWorkspace.shared.notificationCenter.addObserver(forName: NSWorkspace.didWakeNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.connectionDidWake() }
        }
        activationObserver = NotificationCenter.default.addObserver(forName: NSApplication.didBecomeActiveNotification, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in
                guard let self, !self.connected, !self.busy else { return }
                self.reloadProfiles()
            }
        }
    }
    func networkChanged(_ status: NWPath.Status, signature: String? = nil) {
        let changed = status == .satisfied && signature != nil && networkSignature != nil && signature != networkSignature
        if let signature { networkSignature = signature }
        lastPath = status
        if status == .unsatisfied && wantsConnection && !waitingForNetwork {
            waitingForNetwork = true; networkCheckTask?.cancel()
            if coreSupportsRetention, let core {
                do { try core.send(["action": "network", "id": UUID().uuidString, "available": false]) }
                catch { scheduleReconnect(immediately: true) }
            } else { scheduleReconnect(immediately: true) }
        } else if status == .satisfied && waitingForNetwork && wantsConnection {
            waitingForNetwork = false
            if coreSupportsRetention, let core {
                do { try core.send(["action": "network", "id": UUID().uuidString, "available": true]) }
                catch { scheduleReconnect(immediately: true) }
            } else { scheduleReconnect(immediately: true) }
        } else if changed { scheduleConnectionCheck(reason: "network") }
    }
    func connectionDidWake() { scheduleConnectionCheck(reason: "wake") }
    private func scheduleConnectionCheck(reason: String) {
        guard wantsConnection, coreSupportsRetention, core != nil, !waitingForNetwork else { return }
        networkCheckTask?.cancel()
        let token = session
        networkCheckTask = Task {
            try? await Task.sleep(nanoseconds: connectionCheckPause)
            guard !Task.isCancelled, session == token, wantsConnection, coreSupportsRetention, !waitingForNetwork, let core else { return }
            let remaining = max(0, 2 - (operationNow - lastCheckAt))
            if remaining > 0 { try? await Task.sleep(nanoseconds: UInt64(remaining * 1_000_000_000)) }
            guard !Task.isCancelled, session == token, wantsConnection, !waitingForNetwork else { return }
            do { try core.send(["action": "check", "id": UUID().uuidString, "reason": reason]); lastCheckAt = operationNow }
            catch { scheduleReconnect(immediately: true) }
        }
    }
    func reloadProfiles(interactionAllowed: Bool = false) {
        guard !loadingProfiles else { return }; loadingProfiles = true
        Task {
            do {
                let loader = configurationLoader
                let loaded = try await Task.detached { try loader(interactionAllowed) }.value
                profiles = loaded; storageError = false
                if message.contains("钥匙串") { message = "" }
            } catch {
                storageError = true; message = "需要允许钥匙串访问。点击“解锁我的连接”后，在系统窗口中确认。"
            }
            loadingProfiles = false
        }
    }

    func log(_ text: String) {
        activity.insert(Date().formatted(date: .omitted, time: .standard) + "  " + text, at: 0)
        if activity.count > 40 { activity.removeLast() }
    }
    func toggle() { if connected || busy { disconnect() } else if storageError { reloadProfiles(interactionAllowed: true) } else if profiles.isEmpty { showImport = true } else { connect() } }
    func connect() {
        guard !connected, !busy, !diagnosticRunning, let profile = selected else { return }
        fallbackNotice = nil
        let id = UUID(); preflightID = id; checkingPreflight = true; showNetworkCheck = false
        let shadowrocket = NSWorkspace.shared.runningApplications.contains { $0.bundleIdentifier == "com.liguangming.Shadowrocket" }
        preflightTask = Task {
            let report = await preflightInspector(profile, httpPort, shadowrocket)
            guard !Task.isCancelled, preflightID == id else { return }
            checkingPreflight = false; preflightID = nil; networkPreflight = report
            if report.requiresAcknowledgement { showNetworkCheck = true; return }
            startConnection()
        }
    }
    func continueAfterNetworkCheck() {
        guard !checkingPreflight, !connected, !busy else { return }
        guard !usesPacketTunnel || networkPreflight?.requiresAcknowledgement != true else { return }
        showNetworkCheck = false; startConnection()
    }
    func cancelNetworkCheck() { showNetworkCheck = false; networkPreflight = nil }
    private func startConnection() {
        guard let profile = selected else { return }
        guard prepareAITunnel() else { return }
        wantsConnection = true; backoff.reset(); waitingForNetwork = false
        session = UUID(); let token = session
        startProgress(.connect, phase: core != nil || recoveryNeeded ? .restoring : .server)
        if lastPath == .unsatisfied { scheduleReconnect(immediately: true); return }
        if core != nil || recoveryNeeded || cleanupTask != nil {
            state = .disconnecting
            Task {
                let restored = await clean()
                guard session == token, wantsConnection else { return }
                guard restored else { finishProgress(); state = .failed; wantsConnection = false; return }
                begin(profile)
            }
        } else { begin(profile) }
    }
    private func begin(_ profile: Profile) {
        clearExitVerification(); clearSpeedTest(); networkCheckTask?.cancel()
        coreSupportsRetention = false; transportReady = false; proxyApplied = false; proxyRetainedDuringRecovery = false; packetTunnelActive = false
        advanceProgress(.server); retryUntil = nil
        session = UUID(); let token = session
        state = backoff.attempts > 0 ? .reconnecting : .connecting; message = "正在验证加密连接…"
        lastFailureRetryable = true; statusNote = nil; waitingForNetwork = false
        latency = nil; connectedAt = nil; upload = 0; download = 0
        let child = JSONChild(executable: coreExecutable, arguments: ["run"])
        core = child
        child.onEvent = { [weak self] event in guard let self, self.session == token else { return }; self.coreEvent(event, token: token) }
        child.onExit = { [weak self] code in
            guard let self, self.session == token, self.wantsConnection else { return }
            self.log("连接内核已停止")
            if self.reconnect && self.lastFailureRetryable { self.scheduleReconnect() } else { self.fail(self.statusNote ?? "连接已中断，请检查服务器和网络。") }
        }
        do {
            try child.start(); try child.send(["profile": CoreRPC.dictionary(profile), "alternatives": CoreRPC.dictionary(profiles.filter { $0.id != profile.id }), "preferredID": preferredProfileID ?? "", "routingMode": routingMode.coreMode, "proxyDomains": Self.defaultDomains.components(separatedBy: .newlines), "packetTunnel": usesPacketTunnel])
            log("开始连接 · " + profile.protocolName)
            armConnectTimeout(token)
        } catch { fail("连接内核启动失败：" + error.localizedDescription) }
    }
    private func armConnectTimeout(_ token: UUID) {
        connectTimeout?.cancel()
        connectTimeout = Task {
            try? await Task.sleep(nanoseconds: 75_000_000_000)
            guard !Task.isCancelled, session == token, state != .connected else { return }
            fail("连接超时。若系统正在请求权限，请完成确认后重试。")
        }
    }
    private func coreEvent(_ event: [String: Any], token: UUID) {
        switch event["event"] as? String {
        case "transportState":
            coreSupportsRetention = true
            let value = event["state"] as? String ?? "connecting"
            let phase: ConnectionProgress.Phase = event["stage"] as? String == "waiting" ? .waiting : .server
            let retained = event["proxyRetained"] as? Bool == true
            if retained || value == "recovering" || value == "offline" {
                connectTimeout?.cancel(); transportReady = false; latency = nil
                clearExitVerification(); clearSpeedTest()
                state = .reconnecting; proxyRetainedDuringRecovery = proxyApplied
                if connectionProgress?.endedAt != nil || connectionProgress?.kind != .reconnect { startProgress(.reconnect, phase: phase) } else { advanceProgress(phase) }
            }
            waitingForNetwork = value == "offline"
            let delay = (event["retryAfterMs"] as? NSNumber)?.doubleValue ?? 0
            retryUntil = phase == .waiting && !waitingForNetwork ? operationNow + delay / 1000 : nil
            statusNote = waitingForNetwork ? "网络暂不可用" : (event["message"] as? String)
        case "alternate": statusNote = "正在尝试备用连接…"
        case "recovered":
            coreSupportsRetention = true; transportReady = true; waitingForNetwork = false
            preferredProfileID = event["profileID"] as? String
            latency = event["latencyMs"] as? Int; lastHealthAt = Date(); retryUntil = nil; statusNote = nil
            if proxyApplied { finishReadyConnection() } else { advanceProgress(.applying) }
        case "ready":
            packetTunnelActive = usesPacketTunnel && event["packetTunnel"] as? Bool == true
            guard !usesPacketTunnel || packetTunnelActive else { fail("系统隧道未接管 DNS/UDP，已停止连接以避免误报保护状态。"); return }
            armConnectTimeout(token)
            transportReady = true
            if event["retainsProxy"] as? Bool == true { coreSupportsRetention = true }
            preferredProfileID = event["profileID"] as? String
            statusNote = nil
            latency = event["latencyMs"] as? Int; lastHealthAt = Date(); advanceProgress(.applying)
            guard let http = event["httpPort"] as? Int, let socks = event["socksPort"] as? Int, let core else { fail("本地连接信息不完整。"); return }
            httpPort = http; message = "连接验证通过，正在接入系统代理…"; log("加密连接已验证")
            configureAgent(http: http, socks: socks, pid: core.process.processIdentifier, token: token)
        case "progress":
            if event["stage"] as? String == "server" { advanceProgress(.server) }
            else if event["stage"] as? String == "checking" { advanceProgress(.checking) }
            else if event["stage"] as? String == "applying" { advanceProgress(.applying) }
        case "speedProgress", "speedResult": receiveSpeedEvent(event)
        case "exitVerification": receiveExitVerification(event)
        case "stats": upload = (event["uploadBytes"] as? NSNumber)?.int64Value ?? 0; download = (event["downloadBytes"] as? NSNumber)?.int64Value ?? 0
        case "health":
            if event["ok"] as? Bool == true { latency = event["latencyMs"] as? Int; lastHealthAt = Date(); statusNote = nil; message = packetTunnelActive ? protectionNote : "浏览器与遵循系统代理的应用已接入" }
            else { latency = nil; statusNote = "网络检查未响应，正在复查"; message = "连接响应异常，正在检查…"; log("健康检查暂未通过") }
        case "error":
            lastFailureRetryable = event["retryable"] as? Bool ?? true
            let text = event["message"] as? String ?? "连接失败"
            message = text == "context deadline exceeded" ? "连接服务器超时" : text
            if lastFailureRetryable, networkPreflight?.serverUsesVPNRoute == true { message = "握手未完成，服务器入口仍经过其他 VPN。请为入口设置直连后重新检查。" }
            statusNote = message; log(message)
        default: break
        }
    }
    private func finishReadyConnection() {
        guard transportReady, proxyApplied else { return }
        finishProgress(); connectMs = connectionProgress.map { Int($0.elapsed(at: operationNow) * 1000) }
        state = .connected; connectedAt = Date(); backoff.reset(); statusNote = nil; retryUntil = nil
        networkPreflight = nil
        message = packetTunnelActive ? protectionNote : "浏览器与遵循系统代理的应用已接入"; connectTimeout?.cancel()
        proxyOwnershipTask?.cancel()
        systemProxyActive = !usesPacketTunnel && httpPort.map(proxyInspector) == true
        proxyOwnershipTask = Task { [weak self] in
            while !Task.isCancelled {
                try? await Task.sleep(nanoseconds: 2_000_000_000)
                guard !Task.isCancelled, let self, self.connected else { return }
                let active = !self.usesPacketTunnel && self.httpPort.map(self.proxyInspector) == true
                if self.systemProxyActive != active { self.systemProxyActive = active }
            }
        }
        if verifyAfterSetup { verifyAfterSetup = false; verifyExit() }
    }
    private func makeAgent() -> JSONChild { JSONChild(executable: agentExecutable, arguments: []) }
    private func configureAgent(http: Int, socks: Int, pid: Int32, token: UUID) {
        let child = makeAgent(); agent = child
        child.onEvent = { [weak self] event in
            guard let self, self.session == token else { return }
            switch event["event"] as? String {
            case "applied": self.proxyApplied = true; if !self.transportReady && self.coreSupportsRetention { self.proxyRetainedDuringRecovery = true }; self.finishReadyConnection(); self.log(self.packetTunnelActive ? self.protectionNote : "系统代理已接入")
            case "error": self.fail(event["message"] as? String ?? "系统代理设置失败。")
            case "restored": if self.wantsConnection { self.scheduleReconnect() }
            default: break
            }
        }
        child.onExit = { [weak self] _ in
            guard let self, self.session == token, self.wantsConnection, self.state == .connected else { return }
            self.statusNote = "网络接入已停止，正在恢复"; self.scheduleReconnect()
        }
        do { try child.start(); try child.send(["action": usesPacketTunnel ? "monitor" : "enable", "httpPort": http, "socksPort": socks, "corePID": Int(pid)]) }
        catch { fail("无法配置系统代理：" + error.localizedDescription) }
    }
    private func clean() async -> Bool {
        if let pending = cleanupTask { return await pending.value }
        let pending = Task { await performCleanup() }
        cleanupTask = pending
        let restored = await pending.value
        cleanupTask = nil
        return restored
    }
    private func performCleanup() async -> Bool {
        proxyOwnershipTask?.cancel(); proxyOwnershipTask = nil; systemProxyActive = false
        clearExitVerification(); clearSpeedTest()
        if agent?.process.isRunning == false { agent = nil }
        // Keep the tunnel alive until the system proxy is restored.
        if agent == nil && FileManager.default.fileExists(atPath: backupURL.path) {
            let recovery = makeAgent()
            do { try recovery.start(); try recovery.send(["action": "recover"]); agent = recovery }
            catch { message = "网络恢复未完成，现有连接会继续保持。"; return false }
        }
        if let old = agent {
            try? old.send(["action": "disable"]); old.closeInput()
            for _ in 0..<100 { if !old.process.isRunning { break }; try? await Task.sleep(nanoseconds: 100_000_000) }
            if old.process.isRunning {
                // A pending system authorization must never make us tear down
                // the still-live proxy and leave the Mac with a dead route.
                message = "网络恢复仍在等待系统确认，现有连接会继续保持。"
                return false
            }
            agent = nil
        }
        guard !FileManager.default.fileExists(atPath: backupURL.path) else {
            message = "网络恢复未完成，现有连接会继续保持。请检查系统代理写入权限后重试。"
            return false
        }
        if let old = core {
            old.stop()
            for _ in 0..<180 {
                if !old.process.isRunning { break }
                try? await Task.sleep(nanoseconds: 100_000_000)
            }
            guard !old.process.isRunning else { message = "旧连接仍在退出，请稍后重试"; return false }
        }
        core = nil; httpPort = nil; connectedAt = nil
        packetTunnelActive = false
        if hasTunnelRecoveryRecord {
            do { _ = try await CoreRPC.call("recoverTunnel", [:]) }
            catch { message = "系统隧道恢复未完成，请从工具菜单恢复网络。"; return false }
        }
        guard !hasTunnelRecoveryRecord else { message = "系统隧道恢复未完成，请从工具菜单恢复网络。"; return false }
        proxyApplied = false; transportReady = false; coreSupportsRetention = false; proxyRetainedDuringRecovery = false
        return true
    }
    func disconnect() {
        if diagnosticRunning { stopTunnelDiagnostic() }
        preflightTask?.cancel(); preflightID = nil; checkingPreflight = false; showNetworkCheck = false
        wantsConnection = false; waitingForNetwork = false; networkCheckTask?.cancel(); retryTask?.cancel(); connectTimeout?.cancel(); session = UUID(); state = .disconnecting
        let token = session
        startProgress(.disconnect, phase: .restoring)
        Task {
            let restored = await clean()
            guard session == token else { return }
            guard restored else { finishProgress(); state = .failed; statusNote = message; return }
            finishProgress(); state = .idle; statusNote = nil; retryUntil = nil
            message = "已恢复原来的网络设置"; log("连接已断开")
        }
    }
    private func fail(_ text: String) {
        let fallback = usesPacketTunnel && wantsConnection
        wantsConnection = false; waitingForNetwork = false; networkCheckTask?.cancel(); retryTask?.cancel(); connectTimeout?.cancel(); session = UUID()
        let token = session
        state = .disconnecting
        if connectionProgress?.endedAt != nil { startProgress(.disconnect, phase: .restoring) } else { advanceProgress(.restoring) }
        Task {
            let restored = await clean()
            guard session == token else { return }
            if restored, fallback, let profile = selected {
                routingMode = .all; UserDefaults.standard.set("all", forKey: "routingMode")
                fallbackNotice = "AI 防泄漏验证未通过：\(text)。已切换普通代理，AI 保护未启用。"
                wantsConnection = true; backoff.reset(); begin(profile); return
            }
            finishProgress(); state = .failed; retryUntil = nil
            if restored { message = text }; statusNote = message; log(message)
        }
    }
    private func scheduleReconnect(immediately: Bool = false) {
        guard wantsConnection, reconnect, let profile = selected else { return }
        let lastError = statusNote
        networkCheckTask?.cancel(); retryTask?.cancel(); connectTimeout?.cancel(); session = UUID(); state = .reconnecting
        let token = session; let delay = immediately ? 0 : reconnectPause(backoff.nextDelay())
        startProgress(.reconnect, phase: .restoring); statusNote = lastError
        retryTask = Task {
            let restored = await clean()
            guard !Task.isCancelled, wantsConnection, session == token else { return }
            guard restored else { finishProgress(); state = .failed; wantsConnection = false; statusNote = message; return }
            advanceProgress(.waiting)
            guard lastPath != .unsatisfied else {
                waitingForNetwork = true; retryUntil = nil; statusNote = "网络暂不可用"; message = "网络恢复后会自动重连"
                return
            }
            waitingForNetwork = false; retryUntil = operationNow + Double(delay)
            message = "将在 \(Int(ceil(delay))) 秒后重试…"
            try? await Task.sleep(nanoseconds: UInt64(max(0, delay) * 1_000_000_000))
            guard !Task.isCancelled, wantsConnection, session == token else { return }
            if lastPath == .unsatisfied { scheduleReconnect(immediately: true); return }
            begin(profile)
        }
    }
    func recover() {
        guard !connected, !busy else { return }
        if hasTunnelRecoveryRecord {
            do { guard try preparePacketTunnel() else { needsTunnelApproval = true; return } }
            catch { needsTunnelApproval = true; return }
            state = .disconnecting
            Task {
                do { _ = try await CoreRPC.call("recoverTunnel", [:]); state = .idle; recover() }
                catch { state = .failed; message = "系统隧道恢复未完成，请检查后台服务权限后重试。" }
            }
            return
        }
        let child = makeAgent(); agent = child
        child.onEvent = { [weak self] event in
            guard let self else { return }; self.message = event["event"] as? String == "restored" ? "原有网络设置已恢复" : (event["message"] as? String ?? "恢复失败")
            self.log(self.message); self.agent?.closeInput(); self.agent = nil; self.objectWillChange.send()
        }
        do { try child.start(); try child.send(["action": "recover"]) } catch { message = error.localizedDescription }
    }
    func add(name: String, config: String) async throws {
        guard !storageError else { throw CocoaError(.fileWriteNoPermission) }
        let raw = config.trimmingCharacters(in: .whitespacesAndNewlines)
        let kind = raw.hasPrefix("hy2://") || raw.hasPrefix("hysteria2://") ? "hysteria2" : "awg"
        let p = Profile(name: name.isEmpty ? "我的连接" : name, kind: kind, config: raw)
        try await profileValidator(p)
        guard !profiles.contains(where: { $0.config == raw }) else { throw NSError(domain: "Clearline", code: 1, userInfo: [NSLocalizedDescriptionKey: "这条连接已经导入。请为另一台 Mac 使用独立配置。 "]) }
        var updated = profiles; updated.append(p); try profileSaver(updated); profiles = updated; log("连接已保存到本机钥匙串")
    }
    func importCloudProfile(_ profile: Profile) async throws {
        guard !connected, !busy, !storageError else { throw CocoaError(.fileWriteNoPermission) }
        try await profileValidator(profile)
        var updated = profiles.filter { $0.id != profile.id && $0.config != profile.config }
        updated.insert(profile, at: 0)
        try profileSaver(updated); profiles = updated
    }
    func export(password: String, url: URL) async throws {
        let result = try await CoreRPC.call("seal", ["profiles": CoreRPC.dictionary(profiles), "password": password])
        guard let string = result.first?["data"] as? String, let data = Data(base64Encoded: string) else { throw CocoaError(.fileWriteUnknown) }
        try data.write(to: url, options: .atomic); try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: url.path)
    }
    func importTransfer(password: String, url: URL) async throws {
        guard !storageError else { throw CocoaError(.fileWriteNoPermission) }
        let data = try Data(contentsOf: url); guard data.count <= 1_100_000 else { throw CocoaError(.fileReadTooLarge) }
        let result = try await CoreRPC.call("unseal", ["data": data.base64EncodedString(), "password": password])
        guard let value = result.first?["profiles"], let imported = try? JSONDecoder().decode([Profile].self, from: JSONSerialization.data(withJSONObject: value)) else { throw CocoaError(.fileReadCorruptFile) }
        var updated = profiles
        for var p in imported where !updated.contains(where: { $0.config == p.config }) { p.id = UUID().uuidString; updated.append(p) }
        try profileSaver(updated); profiles = updated; log("加密迁移文件已导入")
    }
    func quit() {
        if diagnosticRunning { stopTunnelDiagnostic() }
        preflightTask?.cancel(); preflightID = nil
        wantsConnection = false; waitingForNetwork = false; networkCheckTask?.cancel(); retryTask?.cancel(); connectTimeout?.cancel(); session = UUID()
        let token = session
        Task { guard await clean(), session == token else { return }; NSApp.terminate(nil) }
    }
    private func writeAssistantStatus() {
        let country = exitVerification?.country
        let safeCountry = country?.range(of: "^[A-Z]{2}$", options: .regularExpression) != nil ? country : nil
        let error: String? = needsTunnelApproval ? "tunnel_approval" : storageError ? "storage_locked" : state == .failed ? "connection_failed" : showNetworkCheck ? "proxy_conflict" : exitError != nil ? "exit_verification_failed" : nil
        let code: String
        if checkingPreflight { code = "checking_network" }
        else { switch state { case .idle: code = "idle"; case .connecting: code = "connecting"; case .connected: code = "connected"; case .reconnecting: code = "reconnecting"; case .disconnecting: code = "disconnecting"; case .failed: code = "failed" } }
        assistantStatus.updateConnection(.init(loaded: !loadingProfiles && !storageError, profileCount: profiles.count, state: code, routingMode: routingMode.rawValue, phase: connectionProgress?.phase.rawValue, connected: connected, busy: busy, storageLocked: storageError, exitVerified: connected && exitVerification != nil, exitCountry: connected ? safeCountry : nil, networkConflict: networkPreflight?.requiresAcknowledgement == true || (connected && !usesPacketTunnel && !systemProxyActive), errorCode: error, packetTunnelActive: packetTunnelActive, dnsProtected: connected && packetTunnelActive, udpProtected: connected && packetTunnelActive, systemProxyActive: connected && systemProxyActive, diagnosticStage: diagnosticStage, diagnosticPassed: diagnosticPassed))
    }
}
