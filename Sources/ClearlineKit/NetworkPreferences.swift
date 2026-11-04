import Foundation
import SystemConfiguration

public enum NetworkFailure: LocalizedError {
    case authorization, preferences(String)
    public var errorDescription: String? {
        switch self {
        case .authorization: return "当前用户无权管理系统代理，请使用这台 Mac 的管理员账户。"
        case .preferences(let text): return text
        }
    }
}

public enum ProxySnapshot {
    public static let groups = [["HTTPEnable", "HTTPProxy", "HTTPPort"], ["HTTPSEnable", "HTTPSProxy", "HTTPSPort"], ["SOCKSEnable", "SOCKSProxy", "SOCKSPort"], ["ProxyAutoConfigEnable"], ["ProxyAutoDiscoveryEnable"]]
    public static let keys = groups.flatMap { $0 }
    private static func same(_ a: Any?, _ b: Any?) -> Bool {
        if a == nil && b == nil { return true }
        guard let a = a as? NSObject, let b = b as? NSObject else { return false }
        return a.isEqual(b)
    }
    public static func restoring(current: [String: Any], before: [String: Any], installed: [String: Any]) -> [String: Any] {
        var result = current
        // Treat each proxy endpoint as one unit. A later host-only change by
        // another application must not get paired with an old restored port.
        for group in groups where group.allSatisfy({ same(current[$0], installed[$0]) }) {
            for key in group { result[key] = before[key] }
        }
        return result
    }
}

/// Read preferences with SystemConfiguration; apply changes through Apple's
/// supported networksetup utility. No per-session Authorization Services right
/// is held, so disconnect and crash recovery do not wait on an auth dialog.
public final class NetworkPreferences {
    private let backupURL: URL
    public init(backupURL: URL) { self.backupURL = backupURL }
    private struct Service { let id: String; let name: String; let config: [String: Any] }
    private func services() throws -> [Service] {
        guard let prefs = SCPreferencesCreate(nil, "Clearline" as CFString, nil), let all = SCNetworkServiceCopyAll(prefs) as? [SCNetworkService] else { throw NetworkFailure.preferences("无法读取网络服务。") }
        return all.compactMap { service in
            guard SCNetworkServiceGetEnabled(service), let rawID = SCNetworkServiceGetServiceID(service), let rawName = SCNetworkServiceGetName(service), let iface = SCNetworkServiceGetInterface(service), let type = SCNetworkInterfaceGetInterfaceType(iface) as String?, ["Ethernet", "IEEE80211", "Bridge", "Bond"].contains(type), let proto = SCNetworkServiceCopyProtocol(service, kSCNetworkProtocolTypeProxies) else { return nil }
            return Service(id: rawID as String, name: rawName as String, config: SCNetworkProtocolGetConfiguration(proto) as? [String: Any] ?? [:])
        }
    }
    @discardableResult private func command(_ args: [String]) throws -> String {
        let process = Process(); process.executableURL = URL(fileURLWithPath: "/usr/sbin/networksetup"); process.arguments = args
        let output = Pipe(); process.standardOutput = output; process.standardError = output; process.standardInput = FileHandle.nullDevice
        let finished = DispatchSemaphore(value: 0); process.terminationHandler = { _ in finished.signal() }
        try process.run()
        if finished.wait(timeout: .now() + 8) == .timedOut { process.terminate(); throw NetworkFailure.preferences("系统网络命令未完成，请稍后重试恢复网络。") }
        let text = String(decoding: output.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        guard process.terminationStatus == 0, !text.contains("** Error"), !text.localizedCaseInsensitiveContains("requires administrator") else { throw NetworkFailure.authorization }
        return text
    }
    private func number(_ config: [String: Any], _ key: String) -> Int { (config[key] as? NSNumber)?.intValue ?? 0 }
    private func setEndpoint(_ service: String, prefix: String, setter: String, state: String, config: [String: Any]) throws {
        let host = config[prefix + "Proxy"] as? String ?? ""
        let port = number(config, prefix + "Port")
        if !host.isEmpty && (1...65535).contains(port) { try command([setter, service, host, String(port)]) }
        try command([state, service, number(config, prefix + "Enable") == 1 ? "on" : "off"])
    }
    public func enable(httpPort: Int, socksPort: Int) throws {
        guard (1024...65535).contains(httpPort), (1024...65535).contains(socksPort) else { throw NetworkFailure.preferences("本地端口无效。") }
        if FileManager.default.fileExists(atPath: backupURL.path) { try restore() }
        let all = try services()
        guard !all.isEmpty else { throw NetworkFailure.preferences("没有可配置的 Wi-Fi 或以太网服务。") }
        let settings: [String: Any] = ["HTTPEnable": 1, "HTTPProxy": "127.0.0.1", "HTTPPort": httpPort, "HTTPSEnable": 1, "HTTPSProxy": "127.0.0.1", "HTTPSPort": httpPort, "SOCKSEnable": 1, "SOCKSProxy": "127.0.0.1", "SOCKSPort": socksPort, "ProxyAutoConfigEnable": 0, "ProxyAutoDiscoveryEnable": 0]
        var snapshots: [String: Any] = [:]
        for service in all {
            // Authentication passwords belong to the system keychain. Do not
            // replace an authenticated proxy whose credentials we cannot restore.
            for getter in ["-getwebproxy", "-getsecurewebproxy", "-getsocksfirewallproxy"] {
                let info = try command([getter, service.name])
                if info.contains("Authenticated Proxy Enabled: 1") { throw NetworkFailure.preferences("现有网络使用认证代理，轻连已保留该配置。请先在系统设置中另存或切换网络服务。") }
            }
            snapshots[service.id] = ["name": service.name, "before": service.config, "installed": settings]
        }
        let data = try PropertyListSerialization.data(fromPropertyList: snapshots, format: .binary, options: 0)
        try FileManager.default.createDirectory(at: backupURL.deletingLastPathComponent(), withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
        try data.write(to: backupURL, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: backupURL.path)
        do {
            for service in all {
                try command(["-setwebproxy", service.name, "127.0.0.1", String(httpPort)])
                try command(["-setsecurewebproxy", service.name, "127.0.0.1", String(httpPort)])
                try command(["-setsocksfirewallproxy", service.name, "127.0.0.1", String(socksPort)])
                // Preserve an already-disabled setting without another system commit.
                if number(service.config, "ProxyAutoConfigEnable") != 0 { try command(["-setautoproxystate", service.name, "off"]) }
                if number(service.config, "ProxyAutoDiscoveryEnable") != 0 { try command(["-setproxyautodiscovery", service.name, "off"]) }
            }
        } catch { try? restore(); throw error }
    }
    public func restore() throws {
        guard FileManager.default.fileExists(atPath: backupURL.path) else { return }
        guard let snapshots = try PropertyListSerialization.propertyList(from: Data(contentsOf: backupURL), format: nil) as? [String: [String: Any]] else { throw NetworkFailure.preferences("网络恢复记录无法读取。") }
        for service in try services() {
            guard let snap = snapshots[service.id], let before = snap["before"] as? [String: Any], let installed = snap["installed"] as? [String: Any] else { continue }
            let restored = ProxySnapshot.restoring(current: service.config, before: before, installed: installed)
            for (prefix, setter, state) in [("HTTP", "-setwebproxy", "-setwebproxystate"), ("HTTPS", "-setsecurewebproxy", "-setsecurewebproxystate"), ("SOCKS", "-setsocksfirewallproxy", "-setsocksfirewallproxystate")] {
                let group = ProxySnapshot.groups.first { $0.contains(prefix + "Enable") }!
                let currentSubset = service.config.filter { group.contains($0.key) } as NSDictionary
                let nextSubset = restored.filter { group.contains($0.key) } as NSDictionary
                if !currentSubset.isEqual(nextSubset) { try setEndpoint(service.name, prefix: prefix, setter: setter, state: state, config: restored) }
            }
            if number(restored, "ProxyAutoConfigEnable") != number(service.config, "ProxyAutoConfigEnable") { try command(["-setautoproxystate", service.name, number(restored, "ProxyAutoConfigEnable") == 1 ? "on" : "off"]) }
            if number(restored, "ProxyAutoDiscoveryEnable") != number(service.config, "ProxyAutoDiscoveryEnable") { try command(["-setproxyautodiscovery", service.name, number(restored, "ProxyAutoDiscoveryEnable") == 1 ? "on" : "off"]) }
        }
        try FileManager.default.removeItem(at: backupURL)
    }
}
