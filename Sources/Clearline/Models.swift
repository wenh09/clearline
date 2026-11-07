import Foundation
import Security
import ClearlineKit

struct Profile: Codable, Identifiable, Hashable {
    var id: String = UUID().uuidString
    var name: String
    var kind: String
    var config: String
    var protocolName: String { kind == "awg" ? "AmneziaWG" : "Hysteria 2" }
    var endpoint: String {
        if kind == "hysteria2", let url = URL(string: config) { return url.host ?? "服务器" }
        return config.components(separatedBy: .newlines).first { $0.trimmingCharacters(in: .whitespaces).lowercased().hasPrefix("endpoint") }?.components(separatedBy: "=").dropFirst().joined(separator: "=").trimmingCharacters(in: .whitespaces) ?? "服务器"
    }
}

enum KeychainAccess {
    private static let accessLock = NSLock()

    // The login keychain uses the legacy macOS API. Its interaction setting
    // also needs to be disabled for a noninteractive startup read; the SecItem
    // query flag alone can leave a legacy item waiting for an invisible prompt.
    // Serialize all our access while changing this process-local setting.
    static func withInteraction<T>(_ allowed: Bool, operation: () throws -> T) throws -> T {
        accessLock.lock(); defer { accessLock.unlock() }
        var previous: DarwinBoolean = true
        guard SecKeychainGetUserInteractionAllowed(&previous) == errSecSuccess,
              SecKeychainSetUserInteractionAllowed(allowed) == errSecSuccess else {
            throw CocoaError(.fileReadNoPermission)
        }
        defer { SecKeychainSetUserInteractionAllowed(previous.boolValue) }
        return try operation()
    }
}

enum KeychainStore {
    static let service = "org.clearline.client.profiles"
    static func load(interactionAllowed: Bool = false) throws -> [Profile] {
        try KeychainAccess.withInteraction(interactionAllowed) {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "profiles", kSecReturnData as String: true, kSecMatchLimit as String: kSecMatchLimitOne, kSecUseAuthenticationUI as String: interactionAllowed ? kSecUseAuthenticationUIAllow : kSecUseAuthenticationUIFail]
        var result: CFTypeRef?
        let code = SecItemCopyMatching(query as CFDictionary, &result)
        if code == errSecItemNotFound { return [] }
        guard code == errSecSuccess, let data = result as? Data else { throw CocoaError(.fileReadNoPermission) }
        return try JSONDecoder().decode([Profile].self, from: data)
        }
    }
    static func save(_ profiles: [Profile]) throws {
        try KeychainAccess.withInteraction(true) {
        let data = try JSONEncoder().encode(profiles)
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "profiles"]
        let code = SecItemUpdate(query as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        if code == errSecItemNotFound {
            var item = query; item[kSecValueData as String] = data; item[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            guard SecItemAdd(item as CFDictionary, nil) == errSecSuccess else { throw CocoaError(.fileWriteNoPermission) }
        } else if code != errSecSuccess { throw CocoaError(.fileWriteNoPermission) }
        }
    }
}

enum CoreRPC {
    static var coreURL: URL { Bundle.main.bundleURL.appendingPathComponent("Contents/Helpers/clearline-core") }
    static func dictionary(_ value: some Encodable) throws -> Any { try JSONSerialization.jsonObject(with: JSONEncoder().encode(value)) }
    static func call(_ operation: String, _ request: [String: Any]) async throws -> [[String: Any]] {
        try await Task.detached {
            let process = Process(); process.executableURL = coreURL; process.arguments = [operation]
            let input = Pipe(), output = Pipe(); process.standardInput = input; process.standardOutput = output; process.standardError = FileHandle.nullDevice
            try process.run()
            var data = try JSONSerialization.data(withJSONObject: request); data.append(10)
            try input.fileHandleForWriting.write(contentsOf: data); try input.fileHandleForWriting.close()
            let result = output.fileHandleForReading.readDataToEndOfFile(); process.waitUntilExit()
            let events = result.split(separator: 10).compactMap { try? JSONSerialization.jsonObject(with: Data($0)) as? [String: Any] }
            if let error = events.first(where: { $0["event"] as? String == "error" }) { throw NSError(domain: "Clearline", code: 1, userInfo: [NSLocalizedDescriptionKey: error["message"] as? String ?? "操作失败"]) }
            guard process.terminationStatus == 0 else { throw CocoaError(.executableRuntimeMismatch) }
            return events
        }.value
    }
}

struct SiteSpeed: Decodable {
    let samples: Int?
    let minMs: Int?
    let maxMs: Int?
    let warmMs: Int?
    let warmSamples: Int?
    let skipped: Bool
    let route: String
    let responseMs: Int
    let connectMs: Int
    let tlsMs: Int
    let status: Int
    let ok: Bool
    let limited: Bool
    let message: String
}
struct DownloadSpeed: Decodable {
    let bodyMbps: Double?
    let bodyDurationMs: Int?
    let route: String
    let mbps: Double
    let bytes: Int
    let durationMs: Int
    let ok: Bool
    let message: String
}
struct NetworkSpeedReport: Decodable {
    let claude: SiteSpeed?
    let ordinary: SiteSpeed?
    let download: DownloadSpeed?
    let checkedAt: String
}
