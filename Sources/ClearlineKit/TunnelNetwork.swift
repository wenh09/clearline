import Foundation
import Darwin

public struct TunnelNetworkFailure: LocalizedError {
    public let errorDescription: String?
    public init(_ message: String) { errorDescription = message }
}

/// Only the privileged daemon calls this with the real command runner. Tests
/// inject a runner and a temporary journal, without changing system settings.
public final class TunnelNetwork {
    public static let journalPath = "/Library/Application Support/Clearline/system-tunnel-state.plist"
    public static let legacyJournalPath = "/var/run/org.clearline.tunnel.state"
    public static let dnsAddress = "198.18.0.2"
    public static let probeAddress = "192.0.2.254"
    private struct DNSSnapshot: Codable { let service: String; let before: [String] }
    private struct Journal: Codable {
        var uid: UInt32; var interface: String; var endpoint: String
        var clientPID: Int32?
        var ownsServerRoute = false
        var gateways: [String] = []
        var routes: [String] = []
        // Optional fields keep journals from earlier components recoverable.
        var directInterface: String?
        var directRoutes: [String]?
        var dns: [DNSSnapshot] = []
    }
    private let journalURL: URL
    private let run: ([String]) throws -> String
    private let lock = NSRecursiveLock()
    private var active: Journal?
    public init(journalURL: URL = URL(fileURLWithPath: TunnelNetwork.journalPath), command: (([String]) throws -> String)? = nil) {
        self.journalURL = journalURL; run = command ?? Self.command
    }
    public static func command(_ arguments: [String]) throws -> String {
        guard let executable = arguments.first, ["/sbin/route", "/sbin/ifconfig", "/usr/sbin/networksetup", "/usr/bin/dscacheutil", "/usr/bin/killall"].contains(executable) else { throw TunnelNetworkFailure("不支持的网络操作") }
        let process = Process(); process.executableURL = URL(fileURLWithPath: executable); process.arguments = Array(arguments.dropFirst())
        process.standardInput = FileHandle.nullDevice
        let pipe = Pipe(); process.standardOutput = pipe; process.standardError = pipe
        let finished = DispatchSemaphore(value: 0); process.terminationHandler = { _ in finished.signal() }
        let operation = URL(fileURLWithPath: executable).lastPathComponent + " " + (executable == "/sbin/route" ? arguments.dropFirst(2).first ?? "" : executable == "/sbin/ifconfig" ? "configure" : arguments.dropFirst().first ?? "")
        try process.run()
        if finished.wait(timeout: .now() + 8) == .timedOut { process.terminate(); throw TunnelNetworkFailure("系统网络操作超时（\(operation)）") }
        let result = String(decoding: pipe.fileHandleForReading.readDataToEndOfFile(), as: UTF8.self)
        guard process.terminationStatus == 0, !result.contains("** Error") else { throw TunnelNetworkFailure("系统网络操作失败（\(operation)），请检查其他 VPN 或后台服务权限") }
        return result
    }
    private func store(_ journal: Journal) throws {
        try prepareDirectory()
        let encoder = PropertyListEncoder(); encoder.outputFormat = .binary
        try encoder.encode(journal).write(to: journalURL, options: .atomic)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: journalURL.path)
        // Persist both the new inode and atomic rename before touching DNS/routes.
        for path in [journalURL.path, journalURL.deletingLastPathComponent().path] {
            let fd = open(path, O_RDONLY | O_NOFOLLOW | O_CLOEXEC)
            guard fd >= 0 else { throw TunnelNetworkFailure("无法同步网络恢复记录") }
            let synced = fsync(fd) == 0
            if path == journalURL.path { _ = fcntl(fd, F_FULLFSYNC) }
            close(fd)
            guard synced else { throw TunnelNetworkFailure("无法同步网络恢复记录") }
        }
        active = journal
    }
    private func prepareDirectory() throws {
        let directory = journalURL.deletingLastPathComponent()
        try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o711])
        if journalURL.path == Self.journalPath {
            let attributes = try FileManager.default.attributesOfItem(atPath: directory.path)
            guard attributes[.type] as? FileAttributeType == .typeDirectory, (attributes[.ownerAccountID] as? NSNumber)?.uint32Value == 0 else { throw TunnelNetworkFailure("网络恢复目录权限无效") }
            try FileManager.default.setAttributes([.posixPermissions: 0o711], ofItemAtPath: directory.path)
        }
    }
    private func fields(_ text: String) -> [String: String] {
        var result: [String: String] = [:]
        for line in text.components(separatedBy: .newlines) { let pieces = line.split(separator: ":", maxSplits: 1); if pieces.count == 2 { result[pieces[0].trimmingCharacters(in: .whitespaces)] = pieces[1].trimmingCharacters(in: .whitespaces) } }
        return result
    }
    private func physicalRoute() throws -> (interface: String, gateway: String) {
        let route = fields(try run(["/sbin/route", "-n", "get", "default"]))
        guard let interface = route["interface"], interface.range(of: "^(en|bridge|bond)[0-9]+$", options: .regularExpression) != nil,
              let gateway = route["gateway"], Self.validIPv4(gateway) else { throw TunnelNetworkFailure("全局隧道需要直连的 Wi-Fi 或以太网入口，请先停用其他 VPN") }
        return (interface, gateway)
    }
    public static func validIPv4(_ text: String) -> Bool {
        var address = in_addr()
        return text.utf8.count <= 15 && inet_pton(AF_INET, text, &address) == 1 && !text.hasPrefix("127.") && text != "0.0.0.0" && UInt8(text.split(separator: ".").first ?? "0").map { $0 < 224 && $0 > 0 } == true
    }
    private func services() throws -> [String] {
        let order = try run(["/usr/sbin/networksetup", "-listnetworkserviceorder"])
        var name: String?; var result: [String] = []
        for line in order.components(separatedBy: .newlines) {
            if line.range(of: "^\\([0-9]+\\) ", options: .regularExpression) != nil, let end = line.firstIndex(of: ")") { name = String(line[line.index(end, offsetBy: 2)...]); continue }
            if let service = name, line.contains("Device: "), let device = line.components(separatedBy: "Device: ").last?.trimmingCharacters(in: CharacterSet(charactersIn: ") ")), device.range(of: "^(en|bridge|bond)[0-9]+$", options: .regularExpression) != nil { result.append(service) }
            if line.hasPrefix("(Hardware Port:") { name = nil }
        }
        guard !result.isEmpty else { throw TunnelNetworkFailure("没有可配置的网络服务") }; return result
    }
    private func dns(_ service: String) throws -> [String] {
        let raw = try run(["/usr/sbin/networksetup", "-getdnsservers", service])
        if raw.hasPrefix("There aren't any DNS Servers set on ") { return [] }
        let values = raw.components(separatedBy: .newlines).filter { !$0.isEmpty }
        guard !values.isEmpty, values.allSatisfy({ value in var a = in6_addr(); var b = in_addr(); return inet_pton(AF_INET6,value,&a) == 1 || inet_pton(AF_INET,value,&b) == 1 }) else { throw TunnelNetworkFailure("无法保存原 DNS 配置") }
        return values
    }
    private func flush() { _ = try? run(["/usr/bin/dscacheutil", "-flushcache"]); _ = try? run(["/usr/bin/killall", "-HUP", "mDNSResponder"]) }
    public func configureProbe(interface: String) throws {
        guard interface.range(of: "^utun[0-9]+$", options: .regularExpression) != nil else { throw TunnelNetworkFailure("诊断网卡名称无效") }
        // Only this temporary device receives an address. Closing both owned
        // FDs removes its connected host route; no physical preferences change.
        _ = try run(["/sbin/ifconfig", interface, "inet", "192.0.2.253", Self.probeAddress, "mtu", "1280", "up"])
        _ = try run(["/sbin/ifconfig", interface, "inet6", "fd7a:115c:a1e0:ffff::1", "prefixlen", "126", "alias"])
    }
    private func verifyEntry(_ endpoint: String) throws {
        let route = fields(try run(["/sbin/route", "-n", "get", endpoint]))
        guard let interface = route["interface"], interface.range(of: "^(en|bridge|bond)[0-9]+$", options: .regularExpression) != nil else {
            throw TunnelNetworkFailure("服务器入口被全局路由接管，已停止以避免隧道循环")
        }
    }
    private func persistentBypass(_ route: [String: String]) -> Bool {
        let flags = route["flags"] ?? ""
        return flags.contains("STATIC") && !flags.contains("WASCLONED") && !flags.contains("IFSCOPE")
    }
    private static let directPrefixes = ["0.0.0.0/1", "128.0.0.0/1"]
    private func scopedRoute(_ prefix: String, _ interface: String) -> [String: String] {
        (try? run(["/sbin/route", "-n", "get", "-inet", prefix, "-ifscope", interface])).map(fields) ?? [:]
    }
    private func physicalBypass(_ route: [String: String], prefix: String, interface: String, gateways: [String], owned: Bool = false) -> Bool {
        let destination = String(prefix.split(separator: "/")[0])
        let flags = route["flags"] ?? ""
        return (route["destination"] == destination || (destination == "0.0.0.0" && route["destination"] == "default"))
            && route["mask"] == "128.0.0.0" && route["interface"] == interface
            && gateways.contains(route["gateway"] ?? "") && flags.contains("STATIC") && flags.contains("IFSCOPE")
            && !flags.contains("WASCLONED") && (!owned || flags.contains("PROTO1"))
    }
    private func verifyDirect(_ journal: Journal, gateway: String) throws {
        guard let interface = journal.directInterface else { return }
        for prefix in Self.directPrefixes {
            guard physicalBypass(scopedRoute(prefix, interface), prefix: prefix, interface: interface, gateways: [gateway], owned: journal.directRoutes?.contains(prefix) == true) else {
                throw TunnelNetworkFailure("普通网页的直连路由不可用，请重新连接")
            }
        }
    }
    public func install(interface: String, endpoint: String, uid: UInt32, clientPID: Int32? = nil) throws {
        lock.lock(); defer { lock.unlock() }
        guard interface.range(of: "^utun[0-9]+$", options: .regularExpression) != nil, Self.validIPv4(endpoint) else { throw TunnelNetworkFailure("系统隧道参数无效") }
        guard !FileManager.default.fileExists(atPath: journalURL.path) else { throw TunnelNetworkFailure("请先恢复上一次系统隧道的网络设置") }
        let physical = try physicalRoute()
        let entryGateway = physical.gateway
        var journal = Journal(uid: uid, interface: interface, endpoint: endpoint, clientPID: clientPID, gateways: [entryGateway], directInterface: physical.interface, directRoutes: [])
        for service in try services() { journal.dns.append(.init(service: service, before: try dns(service))) }
        try store(journal) // Durable intent before the first mutation.
        do {
            let before = fields(try run(["/sbin/route", "-n", "get", endpoint]))
            let flags = before["flags"] ?? ""
            let temporaryOrScoped = flags.contains("WASCLONED") || flags.contains("IFSCOPE")
            if before["destination"] == endpoint, let iface = before["interface"], iface.range(of: "^(en|bridge|bond)[0-9]+$", options: .regularExpression) == nil {
                throw TunnelNetworkFailure("服务器入口仍经过其他 VPN")
            }
            if before["destination"] == endpoint && !temporaryOrScoped {
                guard let iface = before["interface"], !iface.hasPrefix("utun") else { throw TunnelNetworkFailure("服务器入口仍经过其他 VPN") }
            } else {
                // A route-get cache entry is not a persistent, unscoped bypass.
                // Installing broader TUN routes can invalidate those clones.
                journal.ownsServerRoute = true; try store(journal)
                _ = try run(["/sbin/route", "-n", "add", "-host", endpoint, entryGateway])
            }
            _ = try run(["/sbin/ifconfig", interface, "inet", "198.18.0.1", Self.dnsAddress, "mtu", "1280", "up"])
            _ = try run(["/sbin/ifconfig", interface, "inet6", "fd7a:115c:a1e0::1", "prefixlen", "126", "alias"])
            // IP_BOUND_IF alone cannot bypass the unscoped /1 TUN routes on
            // macOS. Matching physical scoped routes serve only bound sockets;
            // unbound DNS/UDP/IPv6 and AI traffic still enter the packet tunnel.
            for prefix in Self.directPrefixes {
                if physicalBypass(scopedRoute(prefix, physical.interface), prefix: prefix, interface: physical.interface, gateways: [entryGateway]) { continue }
                journal.directRoutes?.append(prefix); try store(journal)
                _ = try run(["/sbin/route", "-n", "add", "-inet", prefix, entryGateway, "-ifscope", physical.interface, "-proto1"])
            }
            for prefix in ["0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"] {
                journal.routes.append(prefix); try store(journal)
                _ = try run(["/sbin/route", "-n", "add", prefix.contains(":") ? "-inet6" : "-inet", prefix, "-interface", interface])
            }
            try verifyEntry(endpoint)
            try verifyDirect(journal, gateway: entryGateway)
            for snapshot in journal.dns { _ = try run(["/usr/sbin/networksetup", "-setdnsservers", snapshot.service, Self.dnsAddress]) }
            flush()
        } catch { try? restore(); throw error }
    }
    public func refresh() throws {
        lock.lock(); defer { lock.unlock() }
        guard var journal = active else { return }
        for snapshot in journal.dns {
            guard try dns(snapshot.service) == [Self.dnsAddress] else { throw TunnelNetworkFailure("DNS 设置已被其他软件修改，请重新连接") }
        }
        try verifyEntry(journal.endpoint)
        let physical = try physicalRoute()
        if let interface = journal.directInterface, interface != physical.interface {
            throw TunnelNetworkFailure("网络接口已改变，请重新连接以恢复普通网页直连")
        }
        let current = physical.gateway
        if current != journal.gateways.last {
            let route = fields(try run(["/sbin/route", "-n", "get", journal.endpoint]))
            let stillOwned = route["destination"] == journal.endpoint && journal.gateways.contains(route["gateway"] ?? "") && persistentBypass(route)
            if journal.ownsServerRoute && !stillOwned && route["destination"] == journal.endpoint { throw TunnelNetworkFailure("服务器入口路由已改变，请重新连接") }
            let previousGateways = journal.gateways
            journal.gateways.append(current); try store(journal)
            if let interface = journal.directInterface {
                for prefix in journal.directRoutes ?? [] {
                    let existing = scopedRoute(prefix, interface)
                    guard physicalBypass(existing, prefix: prefix, interface: interface, gateways: previousGateways, owned: true) else { throw TunnelNetworkFailure("普通网页的直连路由已改变，请重新连接") }
                    _ = try run(["/sbin/route", "-n", "change", "-inet", prefix, current, "-ifscope", interface, "-proto1"])
                }
            }
            if journal.ownsServerRoute { _ = try run(["/sbin/route", "-n", stillOwned ? "change" : "add", "-host", journal.endpoint, current]) }
        }
        try verifyDirect(journal, gateway: current)
    }

    public func restore(owner: UInt32? = nil) throws {
        lock.lock(); defer { lock.unlock() }
        if journalURL.path == Self.journalPath, !FileManager.default.fileExists(atPath: journalURL.path), FileManager.default.fileExists(atPath: Self.legacyJournalPath) {
            try prepareDirectory()
            let attributes = try FileManager.default.attributesOfItem(atPath: Self.legacyJournalPath)
            guard attributes[.type] as? FileAttributeType == .typeRegular, (attributes[.ownerAccountID] as? NSNumber)?.uint32Value == 0 else { throw TunnelNetworkFailure("旧网络恢复记录权限无效") }
            try FileManager.default.moveItem(at: URL(fileURLWithPath: Self.legacyJournalPath), to: journalURL)
        }
        guard FileManager.default.fileExists(atPath: journalURL.path) else { active = nil; return }
        if journalURL.path == Self.journalPath {
            try prepareDirectory()
            let attributes = try FileManager.default.attributesOfItem(atPath: journalURL.path)
            guard attributes[.type] as? FileAttributeType == .typeRegular,
                  (attributes[.ownerAccountID] as? NSNumber)?.uint32Value == 0,
                  ((attributes[.posixPermissions] as? NSNumber)?.intValue ?? 0o777) & 0o077 == 0 else { throw TunnelNetworkFailure("网络恢复记录权限无效") }
        }
        let journal = try PropertyListDecoder().decode(Journal.self, from: Data(contentsOf: journalURL))
        if let owner, owner != journal.uid { throw TunnelNetworkFailure("另一位用户的隧道正在恢复") }
        let existing = try services()
        for snapshot in journal.dns where existing.contains(snapshot.service) {
            if try dns(snapshot.service) == [Self.dnsAddress] { _ = try run(["/usr/sbin/networksetup", "-setdnsservers", snapshot.service] + (snapshot.before.isEmpty ? ["Empty"] : snapshot.before)) }
        }
        for prefix in journal.routes.reversed() {
            let route = (try? run(["/sbin/route", "-n", "get", prefix.contains(":") ? "-inet6" : "-inet", prefix])).map(fields) ?? [:]
            if route["interface"] == journal.interface { _ = try run(["/sbin/route", "-n", "delete", prefix.contains(":") ? "-inet6" : "-inet", prefix, "-interface", journal.interface]) }
        }
        if let interface = journal.directInterface {
            for prefix in (journal.directRoutes ?? []).reversed() {
                let route = scopedRoute(prefix, interface)
                if physicalBypass(route, prefix: prefix, interface: interface, gateways: journal.gateways, owned: true) {
                    _ = try run(["/sbin/route", "-n", "delete", "-inet", prefix, route["gateway"]!, "-ifscope", interface])
                }
            }
        }
        if journal.ownsServerRoute {
            let route = (try? run(["/sbin/route", "-n", "get", journal.endpoint])).map(fields) ?? [:]
            if route["destination"] == journal.endpoint, journal.gateways.contains(route["gateway"] ?? ""), persistentBypass(route) { _ = try run(["/sbin/route", "-n", "delete", "-host", journal.endpoint]) }
        }
        flush(); try FileManager.default.removeItem(at: journalURL); active = nil
    }
}
