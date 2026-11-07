import AppKit
import SwiftUI
import Security
import ClearlineKit

enum CloudVault {
    static let service = "org.clearline.digitalocean"
    static func load(interactionAllowed: Bool = false) throws -> Data? {
        try KeychainAccess.withInteraction(interactionAllowed) {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "state", kSecReturnData as String: true, kSecUseAuthenticationUI as String: interactionAllowed ? kSecUseAuthenticationUIAllow : kSecUseAuthenticationUIFail]
        var value: CFTypeRef?
        let result = SecItemCopyMatching(query as CFDictionary, &value)
        if result == errSecItemNotFound { return nil }
        guard result == errSecSuccess, let data = value as? Data, data.count <= 2_000_000 else { throw CocoaError(.fileReadNoPermission) }
        return data
        }
    }
    static func save(_ data: Data) throws {
        try KeychainAccess.withInteraction(true) {
        let query: [String: Any] = [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service, kSecAttrAccount as String: "state"]
        let result = SecItemUpdate(query as CFDictionary, [kSecValueData as String: data] as CFDictionary)
        if result == errSecItemNotFound {
            var item = query; item[kSecValueData as String] = data
            item[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
            guard SecItemAdd(item as CFDictionary, nil) == errSecSuccess else { throw CocoaError(.fileWriteNoPermission) }
        } else if result != errSecSuccess { throw CocoaError(.fileWriteNoPermission) }
        }
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
