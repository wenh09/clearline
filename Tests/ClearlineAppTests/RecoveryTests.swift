import XCTest
import Network
import AppKit
import SwiftUI
import Darwin
@testable import Clearline

@MainActor final class RecoveryTests: XCTestCase {
    private func fixture(failures: Int, permanent: Bool = false, retained: Bool = false, monitorSystemEvents: Bool = false, pause: @escaping (Int) -> TimeInterval = { Double($0) * 0.005 }, preflight: @escaping (Profile, Int?, Bool) async -> NetworkPreflight = { _, _, _ in .init() }, prepare: @escaping () throws -> Bool = { true }) throws -> (AppModel, URL) {
        UserDefaults.standard.set("ai", forKey: "routingMode")
        let folder = FileManager.default.temporaryDirectory.appendingPathComponent("Clearline-recovery-" + UUID().uuidString)
        try FileManager.default.createDirectory(at: folder, withIntermediateDirectories: true)
        let quoted = "'" + folder.path.replacingOccurrences(of: "'", with: "'\\''") + "'"
        let core = folder.appendingPathComponent("core")
        let agent = folder.appendingPathComponent("agent")
        try """
        #!/bin/sh
        folder=\(quoted)
        IFS= read -r request
        if [ "${1:-}" = diagnoseTunnel ]; then
          printf '%s\\n' '{"event":"diagnosticProgress","stage":"utun_dns_udp"}'
          printf '%s\\n' '{"event":"tunnelDiagnostic","ok":true,"systemChanged":false,"message":"fixture probe passed"}'
          exit 0
        fi
        attempt=0
        if [ -f "$folder/count" ]; then attempt=$(cat "$folder/count"); fi
        attempt=$((attempt+1))
        printf '%s\\n' "$attempt" > "$folder/count"
        if [ "$attempt" -le \(failures) ]; then
          printf '%s\\n' '{"event":"error","message":"fixture failure","retryable":\(permanent ? "false" : "true")}'
          exit 1
        fi
        printf '%s\\n' '{"event":"ready","profileID":"fixture","httpPort":18777,"socksPort":18778,"latencyMs":10,"packetTunnel":true}'
        while IFS= read -r request; do :; done
        """.write(to: core, atomically: true, encoding: .utf8)
        try """
        #!/bin/sh
        folder=\(quoted)
        printf '%s' "$$" > "$folder/agent-pid"
        IFS= read -r request
        printf '%s\\n' '{"event":"applied"}'
        while IFS= read -r request; do
          case "$request" in *disable*|*recover*) sleep 0.04; printf '%s\\n' '{"event":"restored"}'; exit 0;; esac
        done
        printf '%s\\n' '{"event":"restored"}'
        """.write(to: agent, atomically: true, encoding: .utf8)
        if retained {
            try """
            #!/usr/bin/python3
            import sys,json,pathlib,os
            folder=pathlib.Path(\(String(reflecting: folder.path)))
            initial=json.loads(sys.stdin.readline())
            (folder/'initial-routing').write_text(json.dumps({'mode':initial['routingMode'],'domains':initial['proxyDomains'],'packetTunnel':initial.get('packetTunnel',False)}))
            counter=folder/'count'
            counter.write_text(str((int(counter.read_text()) if counter.exists() else 0)+1))
            def emit(event): print(json.dumps(event),flush=True)
            emit({'event':'transportState','state':'connecting','stage':'server','proxyRetained':False})
            emit({'event':'ready','retainsProxy':True,'profileID':'fixture','httpPort':18777,'socksPort':18778,'latencyMs':10,'packetTunnel':initial.get('packetTunnel',False)})
            if (folder/'drop-after-ready').exists():
                emit({'event':'transportState','state':'offline','stage':'waiting','proxyRetained':True})
            for line in sys.stdin:
                command=json.loads(line)
                with (folder/'commands').open('a') as f: f.write(json.dumps(command)+'\\n')
                if command['action']=='network':
                    if not command['available']:
                        emit({'event':'transportState','state':'offline','stage':'waiting','proxyRetained':True})
                    else:
                        emit({'event':'transportState','state':'connecting','stage':'server','proxyRetained':True})
                        emit({'event':'recovered','retainsProxy':True,'profileID':'fixture','httpPort':18777,'socksPort':18778,'latencyMs':12})
                if command['action']=='check': emit({'event':'health','ok':True,'latencyMs':12,'checkID':command['id']})
                if command['action']=='speedTest': emit({'event':'speedResult','id':command['id'],'ok':True,'report':{'claude':{'skipped':True,'route':'tunnel','responseMs':0,'connectMs':0,'tlsMs':0,'status':0,'ok':False,'limited':False,'message':'未检测'},'ordinary':{'samples':3,'minMs':481,'maxMs':537,'warmMs':244,'warmSamples':2,'skipped':False,'route':'tunnel' if initial['routingMode']=='all' else 'direct','responseMs':503,'connectMs':1,'tlsMs':250,'status':200,'ok':True,'limited':False,'message':'已收到响应'},'download':{'route':'tunnel' if initial['routingMode']=='all' else 'direct','bodyMbps':126.0,'bodyDurationMs':317,'mbps':45.6,'bytes':5000000,'durationMs':877,'ok':True,'message':''},'checkedAt':'2026-11-03T10:00:00Z'}})
            """.write(to: core, atomically: true, encoding: .utf8)
        }
        for path in [core, agent] { try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: path.path) }
        // These fixtures neither read Keychain nor touch system network settings.
        let model = AppModel(configurationLoader: { _ in [Profile(id: "fixture", name: "test", kind: "hysteria2", config: "hy2://fixture@127.0.0.1")] }, coreExecutable: core, agentExecutable: agent, recoveryFile: folder.appendingPathComponent("network.plist"), monitorSystemEvents: monitorSystemEvents, reconnectPause: pause, connectionCheckPause: 10_000_000, assistantStatus: AssistantStatusStore(), preflight: preflight, preparePacketTunnel: prepare, proxyInspector: { _ in true })
        return (model, folder)
    }
    private func count(_ folder: URL) -> Int {
        Int((try? String(contentsOf: folder.appendingPathComponent("count"), encoding: .utf8))?.trimmingCharacters(in: .whitespacesAndNewlines) ?? "") ?? 0
    }
    private func until(_ predicate: () -> Bool, timeout: TimeInterval = 6, file: StaticString = #filePath, line: UInt = #line) async {
        let end = Date().addingTimeInterval(timeout)
        while Date() < end {
            if predicate() { return }
            try? await Task.sleep(nanoseconds: 5_000_000)
        }
        XCTFail("Recovery state did not arrive before deadline", file: file, line: line)
    }
    private func loaded(_ model: AppModel) async { await until { !model.loadingProfiles && !model.profiles.isEmpty } }
    private func stop(_ model: AppModel, _ folder: URL) async {
        model.disconnect(); await until { model.state == .idle }
        try? FileManager.default.removeItem(at: folder)
    }
    func testRecoversAfterSevenFailuresAndClearsOldError() async throws {
        let (model, folder) = try fixture(failures: 7)
        await loaded(model); model.connect()
        await until({ model.connected }, timeout: 10)
        XCTAssertEqual(count(folder), 8)
        XCTAssertNil(model.statusNote)
        XCTAssertNil(model.retryUntil)
        await stop(model, folder)
    }
    func testProxyConflictIsShownBeforeTransportAndCanBeRechecked() async throws {
        var checks = 0
        let (model, folder) = try fixture(failures: 0, preflight: { _, _, _ in
            checks += 1
            return checks == 1 ? NetworkPreflight(serverUsesVPNRoute: true, shadowrocketDetected: true, bypassAddress: "192.0.2.10/32") : NetworkPreflight()
        })
        await loaded(model); model.connect()
        await until { model.showNetworkCheck }
        XCTAssertEqual(count(folder), 0)
        XCTAssertFalse(FileManager.default.fileExists(atPath: folder.appendingPathComponent("agent-pid").path))
        model.connect(); await until { model.connected }
        XCTAssertEqual(checks, 2); XCTAssertEqual(count(folder), 1)
        await stop(model, folder)
    }
    func testCancelDuringPreflightNeverStartsLateConnection() async throws {
        let (model, folder) = try fixture(failures: 0, preflight: { _, _, _ in
            try? await Task.sleep(nanoseconds: 100_000_000)
            return NetworkPreflight()
        })
        await loaded(model); model.connect(); model.disconnect()
        await until { model.state == .idle }
        try? await Task.sleep(nanoseconds: 150_000_000)
        XCTAssertEqual(count(folder), 0); XCTAssertFalse(model.checkingPreflight)
        await stop(model, folder)
    }
    func testManualDisconnectCancelsPendingRetry() async throws {
        let (model, folder) = try fixture(failures: 100, pause: { _ in 0.25 })
        await loaded(model); model.connect()
        await until { model.state == .reconnecting && model.retryUntil != nil }
        model.disconnect(); await until { model.state == .idle }
        let attempts = count(folder)
        try? await Task.sleep(nanoseconds: 350_000_000)
        XCTAssertEqual(count(folder), attempts)
        XCTAssertEqual(model.state, .idle)
        await stop(model, folder)
    }
    func testOnlyAIRequiresComponentApproval() async throws {
        let (model, folder) = try fixture(failures: 0, prepare: { false })
        await loaded(model); model.connect(); await until { model.needsTunnelApproval }
        XCTAssertFalse(model.connected); XCTAssertEqual(count(folder), 0)
        model.needsTunnelApproval = false; model.setRoutingMode(.all); model.connect()
        await until { model.connected }; XCTAssertFalse(model.packetTunnelActive)
        XCTAssertEqual(count(folder), 1)
        await stop(model, folder)
    }
    func testMissingPacketProtectionUsesExplicitCompatibilityWarning() async throws {
        let (model, folder) = try fixture(failures: 0)
        let file = folder.appendingPathComponent("core")
        let source = try String(contentsOf: file).replacingOccurrences(of: "\"packetTunnel\":true", with: "\"packetTunnel\":false")
        try source.write(to: file, atomically: true, encoding: .utf8)
        try FileManager.default.setAttributes([.posixPermissions: 0o700], ofItemAtPath: file.path)
        await loaded(model); model.connect()
        await until { model.connected && !model.usesPacketTunnel }
        XCTAssertFalse(model.packetTunnelActive); XCTAssertNotNil(model.fallbackNotice)
        XCTAssertEqual(model.routingMode, .all); XCTAssertTrue(model.fallbackNotice?.contains("AI 保护未启用") == true); XCTAssertEqual(count(folder), 2)
        await stop(model, folder)
    }
    func testComponentApprovalPreservesWorkingCompatibilityUntilProtectedReconnect() async throws {
        var allowed = true
        let (model, folder) = try fixture(failures: 1, permanent: true, prepare: { allowed })
        await loaded(model); model.connect(); await until { model.connected && !model.usesPacketTunnel }
        allowed = false; model.setRoutingMode(.ai)
        XCTAssertTrue(model.needsTunnelApproval); XCTAssertTrue(model.connected)
        XCTAssertEqual(model.routingMode, .all); XCTAssertTrue(!model.usesPacketTunnel); XCTAssertEqual(count(folder), 2)
        allowed = true; model.finishTunnelInstallation()
        await until { model.connected && model.packetTunnelActive && count(folder) == 3 }
        XCTAssertFalse(!model.usesPacketTunnel)
        await stop(model, folder)
    }
    func testDiagnosticKeepsCompatibilityConnectionWithoutClaimingProtection() async throws {
        var allowed = true
        let (model, folder) = try fixture(failures: 0, prepare: { allowed })
        await loaded(model); model.setRoutingMode(.all); model.connect(); await until { model.connected && !model.usesPacketTunnel }
        let pid = try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8)
        allowed = false; model.startTunnelDiagnostic()
        XCTAssertTrue(model.needsTunnelApproval); XCTAssertTrue(model.connected)
        allowed = true; model.finishTunnelInstallation()
        await until { model.diagnosticPassed == true }
        XCTAssertEqual(model.routingMode, .all); XCTAssertTrue(model.connected); XCTAssertFalse(model.packetTunnelActive)
        XCTAssertEqual(count(folder), 1)
        XCTAssertEqual(try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8), pid)
        await stop(model, folder)
    }
    func testOfflineWaitCreatesNoChildAndNetworkReturnReconnects() async throws {
        let (model, folder) = try fixture(failures: 0)
        await loaded(model); model.networkChanged(.unsatisfied); model.connect()
        await until { model.state == .reconnecting && model.statusNote == "网络暂不可用" }
        XCTAssertEqual(count(folder), 0)
        XCTAssertNil(model.retryUntil)
        model.networkChanged(.satisfied)
        await until { model.connected }
        XCTAssertEqual(count(folder), 1)
        await stop(model, folder)
    }
    func testStopDuringNetworkCleanupPreventsResurrection() async throws {
        let (model, folder) = try fixture(failures: 0)
        await loaded(model); model.connect(); await until { model.connected }
        model.networkChanged(.unsatisfied); model.disconnect()
        await until { model.state == .idle }
        model.networkChanged(.satisfied)
        try? await Task.sleep(nanoseconds: 250_000_000)
        XCTAssertEqual(model.state, .idle)
        XCTAssertEqual(count(folder), 1)
        await stop(model, folder)
    }
    func testGuardianExitRestoresConnectionAutomatically() async throws {
        let (model, folder) = try fixture(failures: 0)
        await loaded(model); model.connect(); await until { model.connected }
        let pid = Int32(try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8))!
        XCTAssertEqual(kill(pid, SIGKILL), 0)
        await until { model.connected && self.count(folder) >= 2 }
        XCTAssertNil(model.statusNote)
        await stop(model, folder)
    }
    func testPermanentFailureStopsAfterSingleCompatibilityAttempt() async throws {
        let (model, folder) = try fixture(failures: 100, permanent: true)
        await loaded(model); model.connect(); await until { model.state == .failed }
        try? await Task.sleep(nanoseconds: 250_000_000)
        XCTAssertEqual(count(folder), 2)
        XCTAssertEqual(model.statusNote, "fixture failure")
        await stop(model, folder)
    }
    private func recordedCommands(_ folder: URL) -> [[String: Any]] {
        ((try? String(contentsOf: folder.appendingPathComponent("commands"), encoding: .utf8)) ?? "").split(separator: "\n").compactMap { try? JSONSerialization.jsonObject(with: Data($0.utf8)) as? [String: Any] }
    }
    func testOnlyTwoModesAndOldGlobalKeepsAIProtection() {
        XCTAssertEqual(AppModel.RoutingMode.restoring(nil), .ai)
        XCTAssertEqual(AppModel.RoutingMode.restoring("claude"), .ai)
        XCTAssertEqual(AppModel.RoutingMode.restoring("ai"), .ai)
        XCTAssertEqual(AppModel.RoutingMode.restoring("all"), .all)
        XCTAssertEqual(AppModel.RoutingMode.restoring("global"), .ai)
        XCTAssertEqual(AppModel.RoutingMode.allCases.map(\.title), ["AI 分流", "普通代理"])
    }
    func testAIUsesProtectedTunnelAndOrdinaryUsesSystemProxy() async throws {
        for mode in AppModel.RoutingMode.allCases {
            let (model, folder) = try fixture(failures: 0, retained: true)
            await loaded(model); model.setRoutingMode(mode); model.connect(); await until { model.connected }
            XCTAssertEqual(model.packetTunnelActive, mode == .ai)
            XCTAssertEqual(model.systemProxyActive, mode == .all)
            let record = try JSONSerialization.jsonObject(with: Data(contentsOf: folder.appendingPathComponent("initial-routing"))) as! [String: Any]
            XCTAssertEqual(record["mode"] as? String, mode.coreMode); XCTAssertEqual(record["packetTunnel"] as? Bool, mode == .ai)
            await stop(model, folder)
        }
    }
    func testTunnelFailureRestoresExplicitCompatibilityAndShowsReason() async throws {
        let (model, folder) = try fixture(failures: 1, permanent: true)
        await loaded(model); model.connect()
        await until { model.connected && !model.usesPacketTunnel }
        XCTAssertEqual(count(folder), 2); XCTAssertFalse(model.packetTunnelActive)
        XCTAssertTrue(model.fallbackNotice?.contains("fixture failure") == true)
        XCTAssertEqual(model.routingMode, .all)
        await stop(model, folder)
    }
    func testAISelectionSendsCompleteDomainRulesToCore() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true)
        await loaded(model); model.setRoutingMode(.ai); model.connect(); await until { model.connected }
        let data = try Data(contentsOf: folder.appendingPathComponent("initial-routing"))
        let record = try JSONSerialization.jsonObject(with: data) as! [String: Any]
        XCTAssertEqual(record["mode"] as? String, "ai")
        let domains = record["domains"] as! [String]
        for domain in ["claude.ai", "openai.com", "chatgpt.com", "oaistatic.com", "oaiusercontent.com", "challenges.cloudflare.com"] { XCTAssertTrue(domains.contains(domain)) }
        XCTAssertFalse(domains.contains("cloudflare.com"))
        await stop(model, folder)
    }
    func testSpeedReportSeparatesWarmResponseAndWholeTransfer() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true)
        await loaded(model); model.connect(); await until { model.connected }
        model.startSpeedTest(); await until { !model.isSpeedTesting && model.speedReport != nil }
        XCTAssertEqual(model.speedReport?.ordinary?.samples, 3)
        XCTAssertEqual(model.speedReport?.ordinary?.warmMs, 244)
        XCTAssertEqual(model.speedReport?.download?.mbps, 45.6)
        XCTAssertEqual(model.speedReport?.download?.bodyMbps, 126.0)
        if let path = ProcessInfo.processInfo.environment["CLEARLINE_UI_SNAPSHOT"] {
            let view = NSHostingView(rootView: MainView().environmentObject(model))
            view.frame = NSRect(x: 0, y: 0, width: 360, height: 660)
            view.layoutSubtreeIfNeeded()
            if let bitmap = view.bitmapImageRepForCachingDisplay(in: view.bounds) {
                view.cacheDisplay(in: view.bounds, to: bitmap)
                if let data = bitmap.representation(using: .png, properties: [:]) { try data.write(to: URL(fileURLWithPath: path)) }
            }
        }
        await stop(model, folder)
    }
    func testRetainedRecoveryKeepsAgentAndPorts() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true)
        await loaded(model); model.connect(); await until { model.connected }
        let pid = try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8)
        let port = model.httpPort
        model.networkChanged(.unsatisfied, signature: "offline")
        await until { model.state == .reconnecting && model.proxyRetainedDuringRecovery }
        XCTAssertEqual(model.httpPort, port)
        model.networkChanged(.satisfied, signature: "wifi-b")
        await until { model.connected }
        XCTAssertEqual(count(folder), 1)
        XCTAssertEqual(try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8), pid)
        XCTAssertEqual(model.httpPort, port)
        XCTAssertNil(model.statusNote)
        await stop(model, folder)
    }
    func testWakeAndSameStatusNetworkChangeCoalesceChecks() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true)
        await loaded(model); model.networkChanged(.satisfied, signature: "wifi-a"); model.connect(); await until { model.connected }
        for _ in 0..<10 { model.connectionDidWake() }
        model.networkChanged(.satisfied, signature: "wifi-b")
        await until { self.recordedCommands(folder).filter { $0["action"] as? String == "check" }.count == 1 }
        XCTAssertEqual(count(folder), 1)
        XCTAssertTrue(model.connected)
        await stop(model, folder)
    }
    func testManualStopCancelsQueuedWakeCheck() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true)
        await loaded(model); model.connect(); await until { model.connected }
        model.connectionDidWake(); model.disconnect(); await until { model.state == .idle }
        model.connectionDidWake()
        try? await Task.sleep(nanoseconds: 50_000_000)
        XCTAssertTrue(recordedCommands(folder).filter { $0["action"] as? String == "check" }.isEmpty)
        await stop(model, folder)
    }
    func testProxyApplyDoesNotClaimConnectedWhileTransportIsOffline() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true)
        try Data().write(to: folder.appendingPathComponent("drop-after-ready"))
        await loaded(model); model.connect()
        await until { model.state == .reconnecting && model.proxyRetainedDuringRecovery }
        XCTAssertFalse(model.connected)
        let pid = try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8)
        model.networkChanged(.satisfied, signature: "wifi")
        await until { model.connected }
        XCTAssertEqual(try String(contentsOf: folder.appendingPathComponent("agent-pid"), encoding: .utf8), pid)
        await stop(model, folder)
    }

    func testSimulatedWorkspaceWakeNotificationChecksCurrentConnection() async throws {
        let (model, folder) = try fixture(failures: 0, retained: true, monitorSystemEvents: true)
        await loaded(model); model.connect(); await until { model.connected }
        try? await Task.sleep(nanoseconds: 50_000_000)
        NSWorkspace.shared.notificationCenter.post(name: NSWorkspace.didWakeNotification, object: nil)
        await until { self.recordedCommands(folder).contains { $0["reason"] as? String == "wake" } }
        XCTAssertEqual(count(folder), 1)
        await stop(model, folder)
    }

}
