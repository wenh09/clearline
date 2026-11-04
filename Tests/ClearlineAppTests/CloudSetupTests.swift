import XCTest
import AppKit
import SwiftUI
@testable import Clearline

@MainActor final class CloudSetupTests: XCTestCase {
    func testOfficialAuthorizationOriginOnly() {
        XCTAssertNotNil(CloudSetupModel.validAuthorizationURL("https://cloud.digitalocean.com/v1/oauth/authorize?state=test"))
        for url in ["http://cloud.digitalocean.com/v1/oauth/authorize", "https://cloud.digitalocean.com.evil.example/v1/oauth/authorize", "https://user@cloud.digitalocean.com/v1/oauth/authorize", "https://cloud.digitalocean.com:444/v1/oauth/authorize", "https://cloud.digitalocean.com/login"] {
            XCTAssertNil(CloudSetupModel.validAuthorizationURL(url))
        }
    }
    func testPaidDeploymentRequiresExplicitConsentAndCurrentPlan() {
        let model = CloudSetupModel(load: { nil }, save: { _ in }, open: { _ in false }, writeStatus: false)
        var commands: [[String: Any]] = []; model.sendOverride = { commands.append($0) }
        model.accept(["event": "cloudAuthorized"])
        model.accept(["event": "cloudPlan", "plan": ["id": "plan", "region": "sfo3", "size": "s-1vcpu-1gb", "monthly": 6.0, "hourly": 0.01, "transfer": 1.0, "expires": Int64(Date().timeIntervalSince1970) + 600]])
        model.command("deploy"); XCTAssertTrue(commands.isEmpty)
        model.consent = true; model.command("deploy")
        XCTAssertEqual(commands.count, 1); XCTAssertEqual(commands.first?["id"] as? String, "plan")
    }
    func testCredentialAcknowledgementFollowsSuccessfulKeychainSave() {
        var saved = false, acknowledged = false
        let model = CloudSetupModel(load: { nil }, save: { _ in saved = true }, open: { _ in false }, writeStatus: false)
        model.sendOverride = { command in XCTAssertTrue(saved); acknowledged = command["action"] as? String == "stored" && command["id"] as? String == "v1" }
        model.accept(["event": "cloudState", "id": "v1", "state": ["accessToken": "test-only"]])
        XCTAssertTrue(saved); XCTAssertTrue(acknowledged)
    }
    func testDeniedKeychainNeverAcknowledgesCloudMutation() {
        let model = CloudSetupModel(load: { nil }, save: { _ in throw CocoaError(.fileWriteNoPermission) }, open: { _ in false }, writeStatus: false)
        var acknowledged = false; model.sendOverride = { _ in acknowledged = true }
        model.accept(["event": "cloudState", "id": "v1", "state": ["accessToken": "test-only"]])
        XCTAssertFalse(acknowledged); XCTAssertNotNil(model.error)
    }
    func testNewPlanResetsPriorBillingConsent() {
        let model = CloudSetupModel(load: { nil }, save: { _ in }, open: { _ in false }, writeStatus: false)
        model.consent = true
        model.accept(["event": "cloudPlan", "plan": ["id": "new", "region": "nyc3", "size": "s-1vcpu-1gb", "monthly": 7.0, "hourly": 0.01, "transfer": 1.0, "expires": Int64(Date().timeIntervalSince1970) + 600]])
        XCTAssertFalse(model.consent); XCTAssertEqual(model.plan?.location, "美国 · 纽约")
    }
    func testOnboardingPreviewRendersWithoutAccountOrNetwork() throws {
        let cloud = CloudSetupModel(load: { nil }, save: { _ in }, open: { _ in false }, writeStatus: false)
        cloud.sendOverride = { _ in }
        cloud.accept(["event": "cloudAuthorized"])
        cloud.accept(["event": "cloudPlan", "plan": ["id": "preview", "region": "sfo3", "size": "s-1vcpu-1gb", "monthly": 6.0, "hourly": 0.00893, "transfer": 1.0, "expires": Int64(Date().timeIntervalSince1970) + 600]])
        let app = AppModel(configurationLoader: { _ in [] }, monitorSystemEvents: false)
        let view = NSHostingView(rootView: CloudSetupView(model: cloud).environmentObject(app))
        view.frame = NSRect(x: 0, y: 0, width: 440, height: 630)
        view.layoutSubtreeIfNeeded()
        let rep = try XCTUnwrap(view.bitmapImageRepForCachingDisplay(in: view.bounds))
        view.cacheDisplay(in: view.bounds, to: rep)
        let png = try XCTUnwrap(rep.representation(using: .png, properties: [:]))
        XCTAssertGreaterThan(png.count, 5000)
        if let directory = ProcessInfo.processInfo.environment["CLEARLINE_UI_PREVIEW_DIR"] {
            try FileManager.default.createDirectory(atPath: directory, withIntermediateDirectories: true)
            try png.write(to: URL(fileURLWithPath: directory).appendingPathComponent("digitalocean-plan-preview.png"))
        }
        XCTAssertFalse(cloud.consent)
    }
    func testResumeAllowsRetryAfterConnectionSaveFailure() {
        let model = CloudSetupModel(load: { nil }, save: { _ in }, open: { _ in false }, writeStatus: false)
        model.sendOverride = { _ in }
        let event: [String: Any] = ["event": "cloudDeployed", "profile": ["id": "deployment", "name": "Fixture", "kind": "hysteria2", "config": "test-only"]]
        model.accept(event); XCTAssertTrue(model.working)
        model.working = false // The first profile save was denied by the owner.
        model.command("resume"); XCTAssertNil(model.profile)
        model.accept(event); XCTAssertEqual(model.profile?.id, "deployment")
        model.markSaved(); XCTAssertFalse(model.working); XCTAssertTrue(model.saved)
    }
}
