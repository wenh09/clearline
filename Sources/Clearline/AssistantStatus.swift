import Foundation

/// The connection model owns connection truth. Cloud setup can only update its
/// own section; an old deployment quote cannot overwrite a saved connection.
@MainActor final class AssistantStatusStore {
    static let shared = AssistantStatusStore(file: FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask).first?.appendingPathComponent("Clearline/setup-status.json"))

    struct Connection: Codable, Equatable {
        var loaded: Bool
        var profileCount: Int
        var state: String
        var routingMode: String
        var phase: String?
        var connected: Bool
        var busy: Bool
        var storageLocked: Bool
        var exitVerified: Bool
        var exitCountry: String?
        var networkConflict: Bool
        var errorCode: String?
        var packetTunnelActive = false
        var dnsProtected = false
        var udpProtected = false
        var systemProxyActive = false
        var diagnosticStage: String?
        var diagnosticPassed: Bool?
    }
    struct Cloud: Codable, Equatable {
        var active: Bool
        var intent: String
        var authorized: Bool?
        var working: Bool
        var phase: String
        var existingServerCount: Int
        var serverSelected: Bool
        var hasAppDeployment: Bool
        var needsAttention: Bool
        var quotedMonthlyUSD: Double?
    }
    struct Document: Encodable {
        let schemaVersion = 2
        let version = "0.3.0"
        let build = "5"
        let updatedAt: String
        let connectionSaved: Bool?
        let connected: Bool
        let authorized: Bool?
        let working: Bool
        let needsAttention: Bool
        let connection: Connection?
        let cloud: Cloud?
    }
    private let file: URL?
    private(set) var connection: Connection?
    private(set) var cloud: Cloud?
    private(set) var writeFailed = false

    init(file: URL? = nil) { self.file = file }
    func updateConnection(_ value: Connection) {
        guard connection != value else { return }
        connection = value; write()
    }
    func updateCloud(_ value: Cloud) {
        guard cloud != value else { return }
        cloud = value; write()
    }
    func document() -> Document {
        Document(updatedAt: ISO8601DateFormatter().string(from: Date()),
                 connectionSaved: connection.flatMap { $0.loaded ? $0.profileCount > 0 : nil },
                 connected: connection?.connected ?? false,
                 authorized: cloud?.authorized,
                 working: (connection?.busy ?? false) || (cloud?.active == true && cloud?.working == true),
                 needsAttention: connection?.errorCode != nil || (cloud?.active == true && cloud?.needsAttention == true),
                 connection: connection, cloud: cloud)
    }
    private func write() {
        guard let file else { return }
        do {
            let parent = file.deletingLastPathComponent()
            try FileManager.default.createDirectory(at: parent, withIntermediateDirectories: true, attributes: [.posixPermissions: 0o700])
            let data = try JSONEncoder().encode(document())
            try data.write(to: file, options: .atomic)
            try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: file.path)
            writeFailed = false
        } catch { writeFailed = true }
    }
}
