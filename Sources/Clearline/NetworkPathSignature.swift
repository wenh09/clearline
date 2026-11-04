import Foundation
import Network
import Darwin

enum NetworkPathSignature {
    // Compared in memory only. No interface addresses or Wi-Fi identity are logged.
    static func capture(_ path: NWPath) -> String {
        let interfaces = path.availableInterfaces.filter { path.usesInterfaceType($0.type) }
        let names = Set(interfaces.map(\.name))
        var parts = interfaces.map { "\($0.name):\($0.index)" }
        parts += path.gateways.map { "gateway:\($0)" }
        parts += ["v4:\(path.supportsIPv4)", "v6:\(path.supportsIPv6)", "expensive:\(path.isExpensive)", "constrained:\(path.isConstrained)"]
        var head: UnsafeMutablePointer<ifaddrs>?
        if getifaddrs(&head) == 0 {
            defer { freeifaddrs(head) }
            var current = head
            while let entry = current {
                defer { current = entry.pointee.ifa_next }
                let name = String(cString: entry.pointee.ifa_name)
                guard names.contains(name), let address = entry.pointee.ifa_addr,
                      address.pointee.sa_family == UInt8(AF_INET) || address.pointee.sa_family == UInt8(AF_INET6) else { continue }
                var host = [CChar](repeating: 0, count: Int(NI_MAXHOST))
                if getnameinfo(address, socklen_t(address.pointee.sa_len), &host, socklen_t(host.count), nil, 0, NI_NUMERICHOST) == 0 {
                    parts.append("\(name):\(String(cString: host))")
                }
            }
        }
        return parts.sorted().joined(separator: "|")
    }
}
