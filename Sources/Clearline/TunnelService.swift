import Foundation
import AppKit
import Security

@MainActor enum TunnelService {
    static let receipt = "/Library/PrivilegedHelperTools/org.clearline.tunnel.version.plist"
    static func prepare() throws -> Bool {
        guard FileManager.default.fileExists(atPath: "/var/run/org.clearline.tunnel.sock"),
              let data = try? Data(contentsOf: URL(fileURLWithPath: receipt)),
              let plist = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any],
              let hashes = plist["ClearlineCoreHashes"] as? [String], plist["ClearlineTunnelProtocol"] as? Int == 2 else { return false }
        var core: SecStaticCode?; var info: CFDictionary?
        guard SecStaticCodeCreateWithPath(CoreRPC.coreURL as CFURL, [], &core) == errSecSuccess, let core,
              SecStaticCodeCheckValidity(core, [], nil) == errSecSuccess,
              SecCodeCopySigningInformation(core, SecCSFlags(rawValue: kSecCSSigningInformation), &info) == errSecSuccess,
              let values = info as? [String: Any], let hash = values[kSecCodeInfoUnique as String] as? Data else { return false }
        return hashes.contains(hash.map { String(format: "%02x", $0) }.joined())
    }
    static func openInstaller() {
        let url = Bundle.main.bundleURL.appendingPathComponent("Contents/Resources/ClearlineSystemTunnel.pkg")
        NSWorkspace.shared.open(url)
    }
}
