import XCTest
import AppKit
import SwiftUI
@testable import Clearline

@MainActor final class OnboardingTests: XCTestCase {
    private let server: [String: Any] = ["id": Int64(42), "name": "existing-fixture", "status": "active", "region": "sfo2", "memory": 512, "disk": 10, "monthly": 4.0, "canRead": true]
    private func cloud(status: AssistantStatusStore? = nil, open: @escaping (URL) -> Bool = { _ in true }) -> ConnectionSetupModel {
        ConnectionSetupModel(load: { nil }, save: { _ in }, open: open, writeStatus: false, status: status, persistBrowser: { _ in }, writeClipboard: { _ in }, readClipboard: { "fixture-code" })
    }
    private func until(_ predicate: () -> Bool, file: StaticString = #filePath, line: UInt = #line) async {
        let deadline = Date().addingTimeInterval(3)
        while Date() < deadline {
            if predicate() { return }
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
        XCTFail("Expected onboarding state was not reached", file: file, line: line)
    }
    func testFirstLaunchLoadsExistingServersAndCannotAccidentallyDeploy() {
        let model = cloud(); var requests: [[String: Any]] = []
        model.sendOverride = { requests.append($0) }
        model.accept(["event": "cloudReady", "authorized": true, "canCreate": true])
        XCTAssertEqual(model.intent, .existing)
        XCTAssertEqual(requests.map { $0["action"] as? String }, ["servers"])
        model.accept(["event": "cloudServers", "servers": [server]])
        XCTAssertEqual(model.selectedServer?.monthly, 4)
        XCTAssertEqual(model.selectedServer?.memory, 512)
        model.consent = true
        model.accept(["event": "cloudPlan", "plan": ["id": "stale-new-quote", "region": "sfo3", "size": "other", "monthly": 7.0, "hourly": 0.01, "transfer": 1.0, "expires": Int64(Date().timeIntervalSince1970) + 600]])
        model.command("deploy")
        XCTAssertNil(model.plan); XCTAssertFalse(model.canDeploy)
        XCTAssertFalse(requests.contains { $0["action"] as? String == "deploy" })
        model.command("pairExisting")
        XCTAssertEqual(requests.last?["serverID"] as? Int64, 42)
        XCTAssertEqual(requests.last?["intent"] as? String, "existing")
    }
    func testChangingBrowserReopensSameAuthorizationRatherThanStartingNewLogin() {
        var opened: [URL] = [], requests: [String] = []
        let model = cloud(open: { opened.append($0); return true })
        model.sendOverride = { requests.append($0["action"] as? String ?? "") }
        model.command("login")
        let url = "https://cloud.digitalocean.com/v1/oauth/authorize?state=fixture"
        model.accept(["event": "cloudAuthorize", "url": url, "expires": Date().addingTimeInterval(120).timeIntervalSince1970])
        model.browser = .system; model.reopenAuthorization()
        XCTAssertEqual(opened.count, 2); XCTAssertEqual(opened.first, opened.last)
        XCTAssertEqual(requests, ["login"]); XCTAssertTrue(model.authorizationPending)
        model.accept(["event": "cloudAuthorized", "canCreate": false])
        XCTAssertFalse(model.authorizationPending); XCTAssertTrue(model.authorized)
        XCTAssertFalse(model.canCreatePermission)
    }
    func testPairingOnlyOpensSelectedServerOfficialConsole() {
        var opened = 0
        let model = cloud(open: { _ in opened += 1; return true }); model.sendOverride = { _ in }
        model.accept(["event": "cloudReady", "authorized": true])
        model.accept(["event": "cloudServers", "servers": [server]])
        for url in ["https://evil.example/droplets/42/terminal/ui/", "https://cloud.digitalocean.com/droplets/99/terminal/ui/"] {
            model.accept(["event": "cloudPairing", "command": "read-only-fixture", "consoleURL": url, "expires": Date().addingTimeInterval(120).timeIntervalSince1970])
            model.copyPairingAndOpenConsole()
        }
        XCTAssertEqual(opened, 0)
        model.accept(["event": "cloudPairing", "command": "read-only-fixture", "consoleURL": "https://cloud.digitalocean.com/droplets/42/terminal/ui/?os_user=root", "expires": Date().addingTimeInterval(120).timeIntervalSince1970])
        model.copyPairingAndOpenConsole()
        XCTAssertEqual(opened, 1)
    }
    func testImportedConnectionRemainsSavedWhenCloudSetupHasNoDeployment() async throws {
        let status = AssistantStatusStore()
        var saved: [Profile] = []
        let app = AppModel(configurationLoader: { _ in [] }, monitorSystemEvents: false, assistantStatus: status, preflight: { _, _, _ in .init() }, saveProfiles: { saved = $0 }, validateProfile: { _ in })
        await until { !app.loadingProfiles }
        app.performPrimaryAction(); XCTAssertTrue(app.showCloudSetup)
        let model = cloud(status: status); model.sendOverride = { _ in }
        model.accept(["event": "cloudReady", "authorized": true, "hasDeployment": false])
        let profile = Profile(id: "fixture", name: "private-fixture-name", kind: "hysteria2", config: "hy2://fixture-password@192.0.2.10")
        try await app.importCloudProfile(profile); model.markSaved()
        await until { status.connection?.profileCount == 1 }
        XCTAssertEqual(saved.count, 1)
        XCTAssertEqual(status.document().connectionSaved, true)
        XCTAssertEqual(status.cloud?.hasAppDeployment, false)
        let data = try JSONEncoder().encode(status.document())
        let text = String(decoding: data, as: UTF8.self)
        for secret in ["fixture-password", "192.0.2.10", "private-fixture-name"] { XCTAssertFalse(text.contains(secret)) }
        // The same server read twice updates the existing connection, not a duplicate.
        var again = profile; again.id = "new-import-id"
        try await app.importCloudProfile(again)
        XCTAssertEqual(app.profiles.count, 1)
    }
    func testStatusKeepsConnectionTruthSeparateFromClosedCreateQuote() throws {
        let file = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString).appendingPathComponent("status.json")
        defer { try? FileManager.default.removeItem(at: file.deletingLastPathComponent()) }
        let status = AssistantStatusStore(file: file)
        status.updateConnection(.init(loaded: true, profileCount: 1, state: "connected", routingMode: "ai", phase: "applying", connected: true, busy: false, storageLocked: false, exitVerified: true, exitCountry: "US", networkConflict: false, errorCode: nil))
        status.updateCloud(.init(active: false, intent: "create", authorized: true, working: false, phase: "server_selection", existingServerCount: 1, serverSelected: false, hasAppDeployment: false, needsAttention: true, quotedMonthlyUSD: 7))
        let json = try XCTUnwrap(JSONSerialization.jsonObject(with: Data(contentsOf: file)) as? [String: Any])
        XCTAssertEqual(json["schemaVersion"] as? Int, 2)
        XCTAssertEqual(json["connectionSaved"] as? Bool, true)
        XCTAssertEqual(json["connected"] as? Bool, true)
        XCTAssertEqual(json["needsAttention"] as? Bool, false)
        XCTAssertNil(json["monthlyUSD"])
        let permissions = try FileManager.default.attributesOfItem(atPath: file.path)[.posixPermissions] as? NSNumber
        XCTAssertEqual(permissions?.intValue, 0o600)
    }
    func testLockedAccountDoesNotBlockImportOrClaimUnauthorized() async {
        let status = AssistantStatusStore()
        let model = ConnectionSetupModel(load: { throw CocoaError(.fileReadNoPermission) }, unlock: { throw CocoaError(.fileReadCorruptFile) }, save: { _ in }, open: { _ in true }, writeStatus: false, status: status)
        model.start()
        await until { model.accountLocked && !model.working }
        XCTAssertNil(status.document().authorized)
        XCTAssertEqual(status.cloud?.phase, "account_locked")
        model.selectIntent(.importProfile)
        XCTAssertEqual(model.intent, .importProfile)
        XCTAssertFalse(status.document().needsAttention)
        model.selectIntent(.existing)
        model.unlockAccount()
        await until { !model.working && model.error != nil }
        XCTAssertFalse(model.accountLocked) // The explicit unlock loader was used.
        model.close()
    }
    func testFirstConnectionScreensRenderWithoutAccountsOrNetwork() throws {
        let app = AppModel(configurationLoader: { _ in [] }, monitorSystemEvents: false, assistantStatus: AssistantStatusStore(), preflight: { _, _, _ in .init() })
        let existing = cloud(); existing.sendOverride = { _ in }
        existing.accept(["event": "cloudReady", "authorized": true])
        existing.accept(["event": "cloudServers", "servers": [server]])
        try render(ConnectionSetupView(model: existing).environmentObject(app), name: "existing-server", width: 480, height: 640)
        existing.accept(["event": "cloudPairing", "command": "read-only-fixture", "consoleURL": "https://cloud.digitalocean.com/droplets/42/terminal/ui/?os_user=root", "expires": Date().addingTimeInterval(600).timeIntervalSince1970])
        try render(ConnectionSetupView(model: existing).environmentObject(app), name: "console-pairing", width: 480, height: 640)
        let account = cloud(); account.sendOverride = { _ in }
        account.command("login")
        account.accept(["event": "cloudAuthorize", "url": "https://cloud.digitalocean.com/v1/oauth/authorize?state=fixture", "expires": Date().addingTimeInterval(120).timeIntervalSince1970])
        try render(ConnectionSetupView(model: account).environmentObject(app), name: "authorization", width: 480, height: 640)
    }
    private func render<V: View>(_ view: V, name: String, width: CGFloat, height: CGFloat) throws {
        let host = NSHostingView(rootView: view)
        host.frame = NSRect(x: 0, y: 0, width: width, height: height); host.layoutSubtreeIfNeeded()
        let bitmap = try XCTUnwrap(host.bitmapImageRepForCachingDisplay(in: host.bounds))
        host.cacheDisplay(in: host.bounds, to: bitmap)
        let data = try XCTUnwrap(bitmap.representation(using: .png, properties: [:]))
        XCTAssertGreaterThan(data.count, 5000)
        if let directory = ProcessInfo.processInfo.environment["CLEARLINE_UI_PREVIEW_DIR"] {
            try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
            try data.write(to: URL(fileURLWithPath: directory).appendingPathComponent(name + ".png"))
        }
    }
}
