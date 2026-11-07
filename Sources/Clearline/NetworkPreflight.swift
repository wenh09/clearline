import Foundation
import SystemConfiguration

struct NetworkPreflight: Equatable {
    var otherSystemProxy = false
    var serverUsesVPNRoute = false
    var shadowrocketDetected = false
    var bypassAddress: String?
    var requiresAcknowledgement: Bool { otherSystemProxy || serverUsesVPNRoute }
    var title: String { serverUsesVPNRoute ? "服务器入口正在经过其他 VPN" : "检测到其他系统代理" }
    var explanation: String {
        if serverUsesVPNRoute {
            return "这可能让 Hysteria 2 的 UDP 握手超时。先为服务器入口设置直连，或在准备好备用联网方式后断开其他 VPN，再重新检查。"
        }
        return "连接成功后轻连会接管系统代理。若要停用原代理，建议先断开轻连、停用原代理，再重新连接轻连，让退出时恢复正确的网络设置。"
    }

    static func inspect(profile: Profile, ownHTTPPort: Int?, shadowrocket: Bool) async -> Self {
        await Task.detached {
            let proxy = SCDynamicStoreCopyProxies(nil) as? [String: Any] ?? [:]
            let enabled = (proxy["HTTPEnable"] as? NSNumber)?.boolValue == true || (proxy["HTTPSEnable"] as? NSNumber)?.boolValue == true || (proxy["SOCKSEnable"] as? NSNumber)?.boolValue == true || (proxy["ProxyAutoConfigEnable"] as? NSNumber)?.boolValue == true
            let own = ownHTTPPort != nil && (proxy["HTTPPort"] as? NSNumber)?.intValue == ownHTTPPort && (proxy["HTTPProxy"] as? String) == "127.0.0.1"
            let host: String?
            if profile.kind == "hysteria2" { host = URL(string: profile.config)?.host }
            else { host = URL(string: "udp://" + profile.endpoint)?.host }
            var result = Self(otherSystemProxy: enabled && !own, shadowrocketDetected: shadowrocket)
            guard let host, host.count <= 253, host.range(of: "^[A-Za-z0-9][A-Za-z0-9.:-]*$", options: .regularExpression) != nil else { return result }
            let output = route(host)
            let values = output.components(separatedBy: .newlines).reduce(into: [String: String]()) { values, line in
                let fields = line.split(separator: ":", maxSplits: 1).map { $0.trimmingCharacters(in: .whitespaces) }
                if fields.count == 2 { values[fields[0]] = fields[1] }
            }
            let interface = values["interface"] ?? ""
            result.serverUsesVPNRoute = ["utun", "ppp", "ipsec"].contains { interface.hasPrefix($0) }
            let address = values["destination"] ?? host
            if address.range(of: "^[0-9.]+$", options: .regularExpression) != nil { result.bypassAddress = address + "/32" }
            else if address.contains(":") { result.bypassAddress = address + "/128" }
            return result
        }.value
    }
    private static func route(_ host: String) -> String {
        let process = Process(); process.executableURL = URL(fileURLWithPath: "/sbin/route"); process.arguments = ["-n", "get", host]
        let output = Pipe(); process.standardOutput = output; process.standardError = FileHandle.nullDevice
        let ended = DispatchSemaphore(value: 0)
        process.terminationHandler = { _ in ended.signal() }
        do {
            try process.run()
            if ended.wait(timeout: .now() + 2) == .timedOut { process.terminate(); return "" }
            return String(decoding: output.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        } catch { return "" }
    }
}
