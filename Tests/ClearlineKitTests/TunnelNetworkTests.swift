import XCTest
@testable import ClearlineKit

final class TunnelNetworkTests: XCTestCase {
    private final class FakeNetwork {
        var dns = ["Wi-Fi": ["192.0.2.53", "2001:db8::53"], "Ethernet": []]
        var routes: [String: (String, String)] = [:]
        var mutations: [[String]] = []
        var failPrefix: String?
        var gateway = "192.0.2.1"
        var physicalInterface = "en1"
        var clonedEntry = false
        var taggedScoped = Set<String>()
        func command(_ args: [String]) throws -> String {
            if args.contains("-listnetworkserviceorder") { return "(1) Wi-Fi\n(Hardware Port: Wi-Fi, Device: en1)\n(2) Ethernet\n(Hardware Port: Ethernet, Device: en0)\n(3) Other VPN\n(Hardware Port: VPN, Device: )\n" }
            if args.contains("-getdnsservers") { let name = args.last!; return dns[name, default: []].isEmpty ? "There aren't any DNS Servers set on \(name).\n" : dns[name]!.joined(separator: "\n") }
            if args.contains("-setdnsservers") { mutations.append(args); dns[args[2]] = args[3] == "Empty" ? [] : Array(args.dropFirst(3)); return "" }
            if args[0] == "/sbin/route" {
                let action = args[2]
                let base = args[3] == "-host" || args[3] == "-inet" || args[3] == "-inet6" ? args[4] : args[3]
                let scope = args.firstIndex(of: "-ifscope").map { args[$0 + 1] }
                let key = base + (scope.map { "%" + $0 } ?? "")
                if action == "get" {
                    if let route = routes[key] ?? (scope == nil ? nil : routes[base]) {
                        let scoped = scope != nil && routes[key] != nil
                        let flags = clonedEntry && key == "198.51.100.1" ? "<UP,GATEWAY,HOST,WASCLONED,IFSCOPE>" : "<UP,GATEWAY,STATIC" + (scoped ? ",IFSCOPE" : "") + (taggedScoped.contains(key) ? ",PROTO1" : "") + ">"
                        let destination = base.contains("/") ? String(base.split(separator: "/")[0]) : base
                        return "destination: \(destination)\nmask: \(base.hasSuffix("/1") ? "128.0.0.0" : "default")\ninterface: \(route.0)\ngateway: \(route.1)\nflags: \(flags)\n"
                    }
                    return "destination: default\nmask: default\ninterface: \(physicalInterface)\ngateway: \(gateway)\n"
                }
                if key == failPrefix { failPrefix = nil; throw TunnelNetworkFailure("fixture failure") }
                mutations.append(args)
                if action == "add", scope == nil, key == "0.0.0.0/1", clonedEntry { routes["198.51.100.1"] = nil; clonedEntry = false }
                if (action == "add" || action == "change"), key == "198.51.100.1" { clonedEntry = false }
                if action == "delete" { routes[key] = nil; taggedScoped.remove(key) }
                else { routes[key] = args.contains("-interface") ? (args.last!, "") : (scope ?? "en1", args[5]); if args.contains("-proto1") { taggedScoped.insert(key) } }
                return ""
            }
            mutations.append(args); return ""
        }
    }
    private func journal() -> URL { FileManager.default.temporaryDirectory.appendingPathComponent("Clearline-network-test-" + UUID().uuidString) }
    func testScopedPhysicalBypassAllowsBoundTCPWithoutChangingUnboundTunnel() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        for prefix in ["0.0.0.0/1", "128.0.0.0/1"] {
            XCTAssertEqual(fake.routes[prefix]?.0, "utun42", "unbound traffic must retain tunnel routing")
            XCTAssertEqual(fake.routes[prefix + "%en1"]?.0, "en1", "bound physical sockets otherwise fail ENETUNREACH")
            XCTAssertEqual(fake.routes[prefix + "%en1"]?.1, fake.gateway)
        }
        try manager.restore(owner: 501)
        XCTAssertTrue(fake.routes.isEmpty)
    }
    func testScopedRoutePartialFailureRestoresEarlierIntent() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let original = fake.dns
        fake.failPrefix = "128.0.0.0/1%en1"
        XCTAssertThrowsError(try TunnelNetwork(journalURL: path, command: fake.command).install(interface: "utun42", endpoint: "198.51.100.1", uid: 501))
        XCTAssertTrue(fake.routes.isEmpty); XCTAssertEqual(fake.dns, original)
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path))
    }
    func testExistingScopedRouteIsReusedWithoutClaimingOwnership() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); fake.routes["0.0.0.0/1%en1"] = ("en1", fake.gateway)
        let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        XCTAssertFalse(fake.taggedScoped.contains("0.0.0.0/1%en1"))
        try manager.restore()
        XCTAssertEqual(fake.routes.count, 1)
        XCTAssertEqual(fake.routes["0.0.0.0/1%en1"]?.1, fake.gateway)
    }
    func testReplacedScopedRouteIsPreservedAndRefreshFails() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        fake.taggedScoped.remove("0.0.0.0/1%en1") // Another owner replaces it.
        XCTAssertThrowsError(try manager.refresh())
        try manager.restore()
        XCTAssertEqual(fake.routes.count, 1)
        XCTAssertEqual(fake.routes["0.0.0.0/1%en1"]?.0, "en1")
    }
    func testPhysicalInterfaceChangeRequiresReconnect() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        fake.physicalInterface = "en0"
        XCTAssertThrowsError(try manager.refresh())
        try manager.restore(); XCTAssertTrue(fake.routes.isEmpty)
    }
    func testLegacyJournalWithoutScopedRouteFieldsStillRecovers() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let original = fake.dns
        try TunnelNetwork(journalURL: path, command: fake.command).install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        var plist = try XCTUnwrap(PropertyListSerialization.propertyList(from: Data(contentsOf: path), format: nil) as? [String: Any])
        plist.removeValue(forKey: "directInterface"); plist.removeValue(forKey: "directRoutes")
        for key in Array(fake.routes.keys) where key.contains("%") { fake.routes.removeValue(forKey: key) }
        try PropertyListSerialization.data(fromPropertyList: plist, format: .binary, options: 0).write(to: path)
        try TunnelNetwork(journalURL: path, command: fake.command).restore()
        XCTAssertTrue(fake.routes.isEmpty); XCTAssertEqual(fake.dns, original)
    }
    func testClonedScopedHostCacheDoesNotReplacePersistentEntryBypass() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); fake.clonedEntry = true; fake.routes["198.51.100.1"] = ("en1", "192.0.2.1")
        let network = TunnelNetwork(journalURL: path, command: fake.command)
        try network.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        XCTAssertEqual(fake.routes["198.51.100.1"]?.0, "en1")
        XCTAssertTrue(fake.mutations.contains { $0.prefix(4) == ["/sbin/route", "-n", "add", "-host"] })
        try network.restore(owner: 501)
        XCTAssertTrue(fake.routes.isEmpty)
    }
    func testFailedBypassAddDoesNotDeleteExistingScopedCache() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); fake.clonedEntry = true; fake.routes["198.51.100.1"] = ("en1", "192.0.2.1"); fake.failPrefix = "198.51.100.1"
        XCTAssertThrowsError(try TunnelNetwork(journalURL: path, command: fake.command).install(interface: "utun42", endpoint: "198.51.100.1", uid: 501))
        XCTAssertEqual(fake.routes["198.51.100.1"]?.0, "en1"); XCTAssertTrue(fake.clonedEntry)
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path))
    }
    func testProbeOnlyConfiguresTemporaryDeviceWithoutChangingDNSOrDefaultRoutes() throws {
        let path = journal(); let fake = FakeNetwork(); let original = fake.dns
        try TunnelNetwork(journalURL: path, command: fake.command).configureProbe(interface: "utun42")
        XCTAssertEqual(fake.dns, original); XCTAssertTrue(fake.routes.isEmpty)
        XCTAssertEqual(fake.mutations.count, 2)
        XCTAssertTrue(fake.mutations.allSatisfy { $0[0] == "/sbin/ifconfig" && $0[1] == "utun42" })
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path))
    }
    func testBothIPFamiliesAndDNSRestoreAfterCrash() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let original = fake.dns
        let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        XCTAssertEqual(fake.dns["Wi-Fi"], [TunnelNetwork.dnsAddress]); XCTAssertEqual(fake.dns["Ethernet"], [TunnelNetwork.dnsAddress])
        for prefix in ["0.0.0.0/1", "128.0.0.0/1", "::/1", "8000::/1"] { XCTAssertEqual(fake.routes[prefix]?.0, "utun42") }
        XCTAssertEqual(fake.routes["198.51.100.1"]?.1, "192.0.2.1")
        XCTAssertEqual((try FileManager.default.attributesOfItem(atPath: path.path)[.posixPermissions] as? NSNumber)?.intValue, 0o600)
        // A new instance models an interrupted/restarted helper.
        try TunnelNetwork(journalURL: path, command: fake.command).restore(owner: 501)
        XCTAssertEqual(fake.dns, original); XCTAssertTrue(fake.routes.isEmpty)
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path))
    }
    func testPartialFailureRollsBackOnlyOwnedChanges() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let original = fake.dns
        fake.routes["198.51.100.1"] = ("en1", "192.0.2.1") // Existing route belongs to another app.
        fake.failPrefix = "::/1"
        let manager = TunnelNetwork(journalURL: path, command: fake.command)
        XCTAssertThrowsError(try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501))
        XCTAssertEqual(fake.dns, original); XCTAssertEqual(fake.routes.count, 1)
        XCTAssertEqual(fake.routes["198.51.100.1"]?.0, "en1")
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path))
    }
    func testRebootRestoresDNSWhenKernelRoutesAreAlreadyGone() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let original = fake.dns
        try TunnelNetwork(journalURL: path, command: fake.command).install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        fake.routes.removeAll() // Kernel state vanished, DNS preferences survived.
        try TunnelNetwork(journalURL: path, command: fake.command).restore()
        XCTAssertEqual(fake.dns, original)
        XCTAssertFalse(FileManager.default.fileExists(atPath: path.path))
        XCTAssertFalse(TunnelNetwork.journalPath.hasPrefix("/var/run/"))
    }
    func testUserDNSAndOtherVPNRouteArePreservedOnRestore() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        fake.dns["Wi-Fi"] = ["203.0.113.53"]
        fake.routes["0.0.0.0/1"] = ("utun99", "")
        XCTAssertThrowsError(try manager.refresh())
        try manager.restore(owner: 501)
        XCTAssertEqual(fake.dns["Wi-Fi"], ["203.0.113.53"])
        XCTAssertEqual(fake.routes["0.0.0.0/1"]?.0, "utun99")
    }
    func testEntryBypassTracksGatewayChangeAndRejectsDifferentOwner() throws {
        let path = journal(); defer { try? FileManager.default.removeItem(at: path) }
        let fake = FakeNetwork(); let manager = TunnelNetwork(journalURL: path, command: fake.command)
        try manager.install(interface: "utun42", endpoint: "198.51.100.1", uid: 501)
        fake.gateway = "192.0.2.254"; try manager.refresh()
        XCTAssertEqual(fake.routes["198.51.100.1"]?.1, "192.0.2.254")
        for prefix in ["0.0.0.0/1", "128.0.0.0/1"] { XCTAssertEqual(fake.routes[prefix + "%en1"]?.1, "192.0.2.254") }
        XCTAssertThrowsError(try manager.restore(owner: 502))
        XCTAssertTrue(FileManager.default.fileExists(atPath: path.path))
        try manager.restore(owner: 501); XCTAssertTrue(fake.routes.isEmpty)
    }
    func testUntrustedParametersCannotRunNetworkCommands() throws {
        let fake = FakeNetwork(); let manager = TunnelNetwork(journalURL: journal(), command: fake.command)
        for input in ["192.0.2.1;echo invalid", "0.0.0.0", "127.0.0.1", "ff02::1", "-gateway"] { XCTAssertThrowsError(try manager.install(interface: "utun42", endpoint: input, uid: 501)) }
        XCTAssertThrowsError(try manager.install(interface: "utun42 extra", endpoint: "192.0.2.1", uid: 501))
        XCTAssertTrue(fake.mutations.isEmpty)
    }
}
